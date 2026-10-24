package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel は DingTalk のカスタムボットを実装する。
//
// プラットフォームの特性（ここでの実装方針を決めるもの）：
//   - ボットごとのレート制限は毎分 20 件で、超過分は黙って破棄される（HTTP が 200 のままの可能性もある）。
//     このためレート制限はクライアント側で行う必要がある。DefaultRatePerMin を参照。
//   - セキュリティ設定は3択：署名 / カスタムキーワード / IP 許可リスト。署名は唯一
//     メッセージ内容に依存しないため、署名のみサポートする（3つとも無効の素の webhook もサポートする）。
//   - 成功/失敗とも HTTP 200 を返し、body 内の errcode で区別する。errcode を確認しないと
//     配信失敗を成功として記録してしまう。
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk の Webhook アドレスには access_token が含まれ、それ自体が認証情報なので全体をマスクする。
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 送信先は DingTalk の Webhook アドレスそのもの。アドレス変更時は新しいアドレスに対する署名キーを再度明示する必要がある。
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

// Send はメッセージを1回配信する。リンクがあり単発の場合は ActionCard（ボタン付き）、それ以外は markdown を使う。
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk の markdown 本文には明示的なバイト上限がないが、証拠フィールドの異常な肥大化を防ぐため上限を設ける。
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "詳細を表示",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk は業務エラーを 200 応答に埋め込む。
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk の応答を解析できません: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 は署名検証失敗、310000 はキーワード不一致で、どちらも設定エラーなので、
		// 再試行しても自然には直らない。
		return 0, Permanent(fmt.Errorf("DingTalk エラー %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL は公式の署名規則に従い、webhook に timestamp と sign パラメーターを追加する。
//
// 規則：署名対象文字列 = timestamp + "\n" + secret。HMAC-SHA256 の**キーも secret**で、
// 結果を base64 化してから URL エンコードする。timestamp はミリ秒。secret が空ならそのまま返し、
// 署名を有効にしていないボットにも対応する。
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err はそのまま返さない：url.Parse のエラーテキストには完全なアドレス（access_token を含む）が入る。
		return "", fmt.Errorf("Webhook アドレスを解析できません: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL はアドレスの有効性とプロトコルのサポートを検証し、IP リテラルの送信先が内部ネットワークかを判定する。
//
// 注意点は2つ：
//
//  1. **エラーメッセージは必ず機密情報を除去する**。url.Parse 自身は *url.Error を返し、その Error() には
//     **完全な元のアドレス**が含まれる。この機能の各サービスのアドレスには認証情報（DingTalk の access_token、
//     WeCom の key、Telegram の bot token、Feishu の hook id）が埋め込まれている。以前はここで直接 `return err` としていたため、
//     「アドレス形式が不正」というエラーが認証情報を持ち出し、テスト API の 400 応答、
//     各配信でデータベースに保存する last_error、サーバーログ、配信履歴 API に流れていた。
//
//  2. **IP リテラルは直接内部ネットワークか判定**し、ドメイン名はダイヤル時に判定する（blockInternalDial が最終的な
//     適用箇所で、DNS リバインディングにも対応する）。ここで一度行うのは、初回配信の失敗まで待つのでなく、
//     設定保存時点で通知できるようにするため。
//
// プロトコル制限は防御的なもの：file:///gopher:// のようなものは http.Client に予期しない
// 動作を引き起こす（scheme チェックですでに遮断されるが、この範囲を開放する理由はない）。
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("アドレスを解析できません（%s）", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http/https のみ対応しています。指定値: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("ホスト名がありません")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("ローカル / リンクローカルアドレス %s への配信を拒否しました（ローカルサービスへの配信が本当に必要な場合は %s=1 を設定）", ip, AllowLocalTargetsEnv)
	}
	return nil
}
