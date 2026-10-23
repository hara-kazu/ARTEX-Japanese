package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel は汎用 Webhook アダプター：ユーザーが URL、メソッド、リクエストヘッダー、JSON テンプレートを指定する。
// これにより、Slack / Mattermost / Discord / 自前システムにそれぞれ実装を用意する必要がなく、
// 設定可能なテンプレート1つでそれらのプラットフォームをカバーできる。
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 汎用 Webhook に公式の制限はなく、0 を返してデフォルトで制限なしを示す。ユーザーが相手の能力に応じて設定する。
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url と headers をマスクする：送信先アドレス自体に token が含まれることが多く、カスタムヘッダーには通常認証情報があり、
// どちらも API 応答に現れるため、両方を防ぐ必要がある。
// 代償として、編集時に1つのヘッダーを変える場合でも、ヘッダー全体の再入力が必要（マスク値は「元の値を保持」と解釈される）。
// この判断は意図的：もう一度入力してもらうほうを選び、認証情報をブラウザーに返さない。
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 送信先は url。url 変更時は headers の再明示が必要。しないと元の Authorization ヘッダーが
// そのまま新しいアドレスに送信され、これがマスク回避の主経路となる。
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate はテンプレート未入力時のフォールバックのリクエストボディで、単純な JSON 構造により、
// 「JSON を1件受信して保存する」自前の受信側の大半をカバーする。
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData はユーザーテンプレートに公開するコンテキスト。
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt は今回の配信時刻（RFC3339）で、受信側の記録に使う。
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel は「未対応 → 修正済み」のような状態変更の可読な説明。状態変更以外では空。
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("送信先 URL がありません")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("送信先 URL が無効です: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("未対応のメソッド %s（GET/POST/PUT/PATCH を使用可能）", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("リクエスト本文のテンプレートの構文エラー: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET はリクエストボディを含めない：内容を query に詰めるのはテンプレートの能力の範囲外で、GET の意味にも合わないため、
	// GET は「一致したらフックを発火」のような受信側にのみ適する。
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// テンプレートが描画するのは文字列形式の JSON で、ここでは json.RawMessage に変換してそのまま送信し、
		// 二重エスケープでユーザーの丁寧に構築した構造が JSON 文字列に包まれるのを防ぐ。
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("リクエスト本文のテンプレートの出力が有効な JSON ではありません"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 上書きは許可するが、headers の後に適用し、明示的な設定を優先する。
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 汎用 Webhook は本文を切り詰めない（受信側はユーザー自身のサービスで、サイズは body_template によって決まる）ため、
	// バッチ全体を配信済みとする。
	return len(m.Items), nil
}

// renderWebhookBody はユーザーテンプレート（またはデフォルトテンプレート）でリクエストボディを描画する。
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("リクエスト本文のテンプレートの構文エラー: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("リクエスト本文のテンプレートの描画に失敗しました: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate はテンプレートを解析する。
//
// missingkey=zero は存在しない map キーをエラーにせずゼロ値として描画する。ただし、このファイルのコンテキストは構造体なので、
// 主な効果は .Items が空でも range でエラーにしないこと。実際に防ぐべきなのは .Items が nil の場合。
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs はテンプレートに公開する補助関数。
var webhookTemplateFuncs = template.FuncMap{
	// json は任意の値を JSON にシリアライズする。
	//
	// この関数は追加の利便機能ではなく必須：これがないと、ユーザーは {{.Title}} を直接補間するしかなく、
	// 脆弱性のタイトルに引用符や改行が1つでもあればリクエストボディ全体が正しい JSON でなくなり、受信側が
	// 拒否する。エラーメッセージは「JSON 解析失敗」を指すため、タイトルの引用符が原因とはまったく思い至らない。
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons は JSON 断片を別の JSON 文字列値の内部に埋め込むために使う（文字列のエスケープを1段行う）。
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 外側の引用符を除去する：引用符を付けるかは呼び出し元が決める。
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
