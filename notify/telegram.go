package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit は Telegram sendMessage の text フィールドの上限（文字数）。
const telegramTextLimit = 4096

// telegramChannel は Telegram Bot API を実装する。
//
// プラットフォームの特性：
//   - 認証はすべて URL path 内（/bot<token>/sendMessage）で行い、署名は不要。
//   - MarkdownV2 ではなく HTML 解析モードを使う：MarkdownV2 は `_*[]()~`>#+-=|{}.!` の
//     合計 18 文字のエスケープを要求し、1文字でも漏れるとメッセージ全体が拒否される。HTML は & < > の3文字だけでよい。
//   - 業務エラーも HTTP 200 に埋め込まれ、ok フィールドで判定する。
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram は個別チャットで毎秒約 1 件、グループで毎分 20 件。保守的な値を使う。
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token は完全な認証情報。chat_id は受信者にすぎず秘密情報ではない（取得しても Token がなければメッセージを送信できない）。
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url は Token の送信先 API エンドポイント（自前のリバースプロキシなど）を決め、変更時は Token の再明示が必要。
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token がありません")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID がありません")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API アドレスが無効です: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram の応答を解析できません: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 はレート制限で、バックオフ後の再試行が有効。それ以外（400 パラメーター誤り、401 token 誤り、403 ブロック済み、
	// 404 chat 不存在）はすべて設定の問題であり、再試行しても自然に直らない。
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram のレート制限: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram エラー %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint は sendMessage アドレスを組み立てる。base_url が空なら公式 API を使い、
// 空でなければ自前の Bot API リバースプロキシに使う（中国国内のネットワークで一般的な需要）。
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err はそのまま返さない：アドレスには Bot Token が含まれ、この時点では addr すら返してはならない。
		return "", fmt.Errorf("API アドレスの結合に失敗しました（API アドレス: %s）", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML は HTML 本文を描画し、本文と実際に書き込んだ項目数を返す（Channel.Send を参照）。
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram の上限は**文字数**なので、詰める際も文字で計量する（runeSize）。
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">プラットフォームですべて表示</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>状態の変更</b>：%s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>種類</b>：" + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>資産</b>：" + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>要約</b>：" + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">詳細を表示</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes はメッセージタイトルと、必要な場合の切り詰め通知のために確保する（文字単位）。
const telegramReservedRunes = 160

// telegramBatchLine は集約内の1項目を描画する（エスケープ前で、呼び出し元がまとめてエスケープする）。
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle は集約メッセージのタイトル行を描画する。件数は**このメッセージに実際に含まれる**件数を使い、
// バッチ全体の総数ではない。そうしないと読み手は先頭の数値を全件数と思ってしまう。
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("脆弱性のまとめ · 合計%d件", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("（先頭%d件を表示。残り%d件は次の通知に続きます）", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("過去%d分間 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape は HTML テキスト内容をエスケープする。
// Telegram はこの3種類の実体参照のみ認識し、&amp; などの既存の実体参照は二重にエスケープされるが、これこそ
// 期待する動作である。元の文字を表示したいのであり、ユーザーに HTML を注入させたいわけではない。
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr は HTML 属性値をエスケープする。テキストのエスケープに加えて引用符も処理する必要がある。
// URL に引用符が含まれると href 属性が早く閉じ、後ろの内容が注入箇所になってしまうため。
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
