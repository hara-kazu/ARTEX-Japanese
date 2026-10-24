package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets はメッセージをループバック / リンクローカルアドレスに配信できるかを決める。
//
// デフォルトは拒否。これらのアドレス帯に IM ボットや公開メールサーバーは存在せず、アクセスできるものは
// 同じマシンの別サービスの管理ポートや、クラウド環境のメタデータエンドポイントなどの機密性の高い対象である
// （169.254.169.254 ではインスタンスの認証情報を読み取れる）。配信先アドレスは管理者が設定するが、XSS/CSRF
// に悪用された管理セッションや、同じ JWT を共有する別の人が、設定変更によって応答内容を読み取れる。
// doJSON は 4xx/5xx 応答本文の先頭 200 バイトを last_error に書き込み、配信履歴 API が
// それを返すため、半ブラインド読み取りのプリミティブになる。
//
// ただし「ローカル SMTP リレー」（127.0.0.1:25 の postfix）は自前メールの一般的な設定で、
// 一律拒否では利用できなくなる。そのためハードコードで許可せず、明示的な例外の入口を設ける：
// ARTEX_NOTIFY_ALLOW_LOCAL=1 を設定すると許可する。
//
// AllowLocalTargetsEnv として公開するのは、テストで明示的に有効にできるようにするため。本パッケージと server パッケージの
// 多くのケースは 127.0.0.1 の httptest のダミー受信側を使っており、有効にしないとすべてガードに阻止される。
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP は対象 IP が「デフォルトで配信不許可」のアドレス帯に属するかを返す。
//
// 拒否するのはループバック、リンクローカル（クラウドメタデータの 169.254.169.254 を含む）、未指定、マルチキャストのみ。
// RFC1918 のプライベートネットワークは**拒否しない**。内部ネットワークの自前 Mattermost / SMTP リレーはごく普通の正当な用途で、
// それらも拒否すると実環境で機能が使えなくなる。この判断は意図的であり、
// 実際に機密性の高い対象へのアクセスを阻止すると同時に、通常のデプロイを使えなくしてはならない。
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6（::ffff:127.0.0.1）は IPv4 に戻してから判定しないと、チェックを回避される。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial は http.Transport ダイヤラーの Control フックで、**接続確立時**に
// 対象アドレスを確認する。
//
// 設定保存時の検証だけでなくダイヤル段階に設ける理由：ここが最終的な適用箇所であるため。
// 設定検証を回避する2つの状況に対応する。DNS リバインディング（検証時は公開 IP に解決し、
// 実際の接続時は内部ネットワークに解決）とリダイレクト（ホストをまたぐ遷移はすでに拒否するが、同じホスト内の遷移でも
// 別のパスを指す可能性がある）。
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("送信先アドレス %q を解析できません", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("ローカル / リンクローカルアドレス %s への配信を拒否しました（ローカルサービスへの配信が本当に必要な場合は %s=1 を設定）", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport はデフォルトの Transport にダイヤルガードを1つだけ加える。
// Clone によってデフォルトの調整（接続プール、HTTP/2、タイムアウト、proxy など）をすべて保持し、
// チェックを1つ加えるために他の動作を変えることを避ける。
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient はすべてのチャネル配信で共有するクライアント。
//
// プロジェクト全体の外向きプロキシ（server 側の GlobalProxy）は意図的に再利用**しない**。そのプロキシは侵入テスト
// 対象のトラフィック用で、不安定なトンネルであることが多い。通知の可用性が対象ネットワークの揺らぎに左右されてはならない。
// IM のプッシュは直接接続でよい。タイムアウトは 15 秒とし、それ以上遅い相手は実質的に障害状態である。
//
// ホストをまたぐリダイレクトは拒否する：この機能の配信先はすべて「固定の endpoint」という形態で、通常は
// 別ホストへリダイレクトされない。各サービスの認証情報（DingTalk の access_token、WeCom の key、Telegram の
// bot token）は**URL 内にあり**、別ホストへの遷移に追従すると認証情報をリダイレクト先に渡してしまう。同じホスト内の
// 遷移（末尾にスラッシュを付ける場合など）は引き続き許可する。
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("リダイレクトが多すぎます")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("別ホストへのリダイレクトを拒否しました（%s → %s）", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit は読み取る応答ボディの大きさを制限する。相手の異常時は巨大な内容を返す可能性があるが、必要なのは
// 配信履歴に表示するエラーコードと短いエラー説明だけである。
const respBodyLimit = 8 << 10

// doJSON はリクエストを1回送信し、応答ボディ（長さ制限済み）を返す。
//
// payload が nil なら空の body を送信する（GET やプラットフォームが body を要求しない場合）。
// headers 内のキーと値はそのまま追加し、汎用 Webhook のカスタムヘッダーに使う。
//
// エラー分類がこの関数の中心的な責務：ネットワーク層の失敗と 5xx/408/429 は「再試行可能」、
// それ以外の 4xx は「永続的な失敗」とする。403 を再試行しても同じエラーを 3 回ログに記録するだけ。
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// シリアライズ失敗はローカルの bug（設定フィールドの型が違う）で、再試行しても改善しない。
			return nil, Permanent(fmt.Errorf("リクエスト本文の構築に失敗しました: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL が不正。多くはユーザーのアドレスの誤入力で、永続的な失敗にあたる。
		// ここでも err はそのまま返してはならない：url.Parse のエラーテキストには完全なアドレスが含まれる。
		return nil, Permanent(fmt.Errorf("リクエストアドレスが無効です: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 接続拒否、DNS 失敗、タイムアウトは多くが一時的な障害で、バックオフによる再試行に任せる。
		//
		// エラーテキストは必ず機密情報を除去してから外部に渡す。理由：http.Client.Do は *url.Error を返し、
		// その Error() は `Op "完全なURL": 下位のエラー` という形で、この機能の各サービスの認証情報は**URL 内にある**
		// （DingTalk の access_token、WeCom の key、Feishu の hook id、Telegram の /bot<token>/）。
		// 機密情報を除去しないと、認証情報がこのエラー文字列を通って4か所に流れる：notification_deliveries
		// の last_error（平文でデータベース保存）、配信履歴 API の応答（**チャネル設定のマスクを回避**）、
		// サーバーログ、テスト送信 API がフロントエンドに返す 502 テキスト。
		return nil, fmt.Errorf("リクエストに失敗しました: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("レスポンスを読み取れません: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429（レート制限）と 408（タイムアウト）は再試行すべきで、それ以外の 4xx は設定や権限の問題なので再試行しても意味がない。
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("送信先のレート制限またはタイムアウト (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("送信先のサービスエラー (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("送信先がリクエストを拒否しました (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet は応答ボディを短い1行のテキストに圧縮し、エラーメッセージに使う。応答には改行や大量の空白が含まれる可能性があり、
// last_error にそのまま入れると配信履歴画面のレイアウトが崩れる。
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget は配信先アドレスを「scheme://host/…」に圧縮し、エラーメッセージに使う。
//
// これが本パッケージ唯一のアドレスの機密情報除去方針であり、意図的に**十分大胆に**行う：scheme と host 以外は
// すべて破棄する。URL のどの部分が認証情報かを判断する「汎用かつ安全」な方法がないため：
//
//	DingTalk   認証情報は query      /robot/send?access_token=xxx
//	WeCom   認証情報は query      /cgi-bin/webhook/send?key=xxx
//	Feishu   認証情報は**パス末尾部分** /open-apis/bot/v2/hook/<hook_id>
//	Telegram 認証情報は**パス途中の部分** /bot<token>/sendMessage
//
// 「必要な部分だけ保持」するにはチャネルごとの処理が必要だが、1社でも漏れると認証情報が流出する。
// host を保持すれば調査には十分（DNS 解決失敗、接続不能、証明書不正を特定できる）で、
// どのボットかはチャネル設定内のマスク末尾のヒントで識別する。
//
// 解析失敗時は固定のプレースホルダーを返す。元の文字列を決して返さない。
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "（アドレスを解析できません）"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError は通信層エラーからアドレスを除去し、下位の原因のみを保持する。
//
// *url.Error の構造は {Op, URL, Err} で、Error() は URL も一緒に出力する。
// ここでは Err フィールドを明示的に取得し、その Error() を回避する。事後の文字列置換より信頼性が高い。
// 置換では URL エンコード/エスケープされたさまざまな形に正しく対応する必要があり、漏れやすいため。
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 不明なエラー", uerr.Op, host)
	}
	// *url.Error 以外（リダイレクトポリシーが返すエラーなど）にもアドレスが含まれる可能性があり、同じ機密情報除去を通す。
	return redactURLsInText(err.Error())
}

// redactURLsInText はテキスト内に現れる http(s) アドレスを機密情報除去済みの形に置き換える。
//
// 構造化フィールドを取得できないエラー（リダイレクトポリシーのエラー、サードパーティーライブラリーのカスタムエラー）のフォールバックに使う。
// http/https 接頭辞のみ識別し、空白と引用符で分割する。アドレスにはこの2種類の文字は含まれない。
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
