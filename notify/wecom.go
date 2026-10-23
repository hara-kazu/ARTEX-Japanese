package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit は WeCom グループボットの markdown content の厳密な上限（バイト、文字ではない）。
// これは6つのチャネルすべてで最も厳しい制限で、TruncateBytes が存在する主な理由でもある。
const weComMarkdownLimit = 4096

// weComChannel は WeCom グループボットを実装する。
//
// プラットフォームの特性：
//   - URL 上の key のみで認証し、署名には非対応。webhook アドレス自体が認証情報のすべてとなる。
//   - markdown content の上限は 4096 **バイト**で、超過時は全体が拒否される（切り詰めではない）。中国語は1文字 3 バイトなので、
//     本文は千数百文字しか書けず、クライアント側での切り詰めが必要。
//   - レート制限は毎分 20 件で、同様にクライアント側で制限する。
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom の認証情報は Webhook の1か所のみ（URL 上の key）で、署名には非対応。
// アドレス全体が認証情報のすべてであり、他にマスクすべきフィールドはない。
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom のフィールドは Webhook の1か所のみで、送信先と認証情報を兼ねるため、「アドレス変更後に残る認証情報」は存在しない。
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 集約バッチは長くなり得る（50 件 × 各1行 + 接頭辞）ため、4096 バイトを超えやすい。
	// プラットフォームのエラーに頼らず、ここで切り詰める。拒否ではバッチ全体を失うが、切り詰めなら先頭の数件は届くため。
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("WeCom の応答を解析できません: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 は API 呼び出し制限超過。プラットフォームのレート制限窓は移動するため、バックオフ後の再試行は有効で、
		// 明示的に再試行可能とする。ここに到達するのはクライアントの rate_per_min 設定が攻めすぎていることを示し、
		// 再試行はあくまでフォールバックで、実際の修正はチャネルのレート制限値を下げること。
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom のレート制限 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 は webhook key の無効を示す。永続的な失敗であり、再試行しても自然に直らない。
		return 0, Permanent(fmt.Errorf("WeCom エラー %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
