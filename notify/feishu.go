package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel は Feishu（Lark を含む）のカスタムボットを対話型カードで実装する。
//
// プラットフォームの特性：
//   - 署名アルゴリズムは DingTalk と**異なり**、非常に間違えやすい。feishuSign のコメントを参照。
//   - DingTalk と同様、HTTP 200 の body に業務エラーを含める（code != 0）。
//   - カードの header は色テンプレートをサポートし、レベルを配色に対応させることで、メッセージ一覧で深刻度が一目で分かる。
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// Feishu のカスタムボットは毎秒約 5 回、毎分換算で 100 回。
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook アドレスの末尾部分がボットの一意識別子で、認証情報にあたる。
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 同様に、Webhook アドレス変更時は新しいアドレスに対する署名キーの再明示が必要。
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 署名パラメーターはメッセージと同じ階層に置き、secret 設定時にのみ含める。
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 一部のバージョンの Feishu hook はこのフィールド名を使うため、併せて対応する。
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Feishu の応答を解析できません: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Feishu エラー %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Feishu エラー %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign は Feishu 公式の規則で署名を計算する。
//
// ここは特に間違えやすい。公式サンプルは
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// つまり **key = timestamp + "\n" + secret、message は空**で、直感的な
// 「key=secret, message=stringToSign」ではない。それは DingTalk のアルゴリズムである。両者のアルゴリズムはちょうど逆なので、
// 他方の実装をなぞると必ず署名検証に失敗する（19021 が返る）。
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate は脆弱性のレベルをカードの header の配色テンプレートに対応させる。
// 不明なレベルは grey を使う。low と混同しないよう blue は使わない。
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes はカード内容の保守的な上限。Feishu はカードのサイズを制限し、超過すると全体を拒否するため、
// JSON ラッパーのオーバーヘッドも考慮し、公式上限を明確に下回る値を使う。
const feishuMaxCardBytes = 24000

// feishuCard は対話型カードを作り、カードと**実際に書き込んだ項目数**を返す。
// kept の用途は markdownBody と同じ：実際にカードに入った項目だけを配信済みとする。
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 先に項目単位で詰めてからヘッダーを組み立てる。ヘッダーには「残り N 件は次のメッセージで続けて送信」と書くため、
		// N は実際に収まった件数から求める必要がある。
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("プラットフォームですべて表示", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("詳細を表示", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines は単一の脆弱性の lark_md 本文を描画する。
//
// lark_md と markdown は同系統のテキスト形式で、同様にリンクと強調を解釈するため、外部由来の
// フィールドはすべて markdownText（単一行化 + エスケープ）を通す。しないと脆弱性のタイトルだけで
// Feishu 内にクリック可能な外部リンクを作れてしまう。
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**状態の変更**：%s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**種類**：%s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**資産**：%s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**要約**：%s", s)
		}
	}
	return out
}

// feishuBatchLine は集約カード内の1項目を描画する。
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
