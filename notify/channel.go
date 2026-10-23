package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel は通知チャネルのアダプター。実装は**ステートレス**でなければならない：同じインスタンスを複数のチャネル
// 設定が並行して再利用し、認証情報はすべて cfg 引数から渡す。
type Channel interface {
	// Kind はチャネルの種類の識別子を返す。レジストリーのキーと一致させる必要がある。
	Kind() string
	// Validate は設定保存時に呼び出し、必須フィールドと形式を検証する。返したエラーは設定者に直接
	// 表示されるため、文言は漠然と「設定が無効」とせず、「どのフィールドが不足しているか」を示す。
	Validate(cfg map[string]any) error
	// Send はメッセージを1回配信し、**実際に届いた項目数**とエラーを返す。
	//
	// 件数を返す理由：各プラットフォームにはメッセージ長の上限があり、集約メッセージにバッチ全体が収まらなければ切り詰められる。
	// 呼び出し元が無条件にバッチ全体を配信済みとすると、切り捨てられた項目が消えてしまう。メッセージに表示されず、
	// 配信履歴でも成功となり、脆弱性が未送信であることをどこからも発見できない。kept を返すことで、
	// 呼び出し元は先頭の kept 件だけを記録し、残りを次のバッチに回す。
	//
	// エラーを返すと配信失敗を示し、*PermanentError は再試行すべきでないことを示す。
	// 失敗時の kept は意味を持たないため、呼び出し元は無視する。
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin はチャネルの公式推奨の毎分配信上限を返し、新しいチャネルインスタンス
	// 作成時のデフォルトのレート制限値とする。0 は既知の制限がないことを示す。
	DefaultRatePerMin() int
	// SecretKeys はチャネル設定内で認証情報にあたるキー名を返す。API 応答時にはこれらのキーの値をマスクし、
	// 更新時にマスク値を受信した場合はデータベース内の元の値を保持する。どのフィールドが認証情報かは実装自身しか把握していない
	// （WeCom は Webhook アドレス全体が認証情報で、DingTalk はその中の secret のみ）。
	// この知識はチャネル側が提供する必要があり、上位層で推測してはならない。
	SecretKeys() []string
	// DestinationKeys はチャネル設定内で「メッセージの送信先」を決めるキー名を返す。
	//
	// SecretKeys と同じくセキュリティに関わるもの：送信先アドレスと認証情報は独立した2組のフィールドであり、
	// 「アドレスのみ変更し、認証情報はそのまま」を許すと、チャネル設定を変更できる誰もがデータベース内の実際の認証情報を
	// 自分の管理するサーバーに送信でき、チャネル設定でのマスクがまったく意味を持たなくなる。
	// 詳細は PrepareConfigUpdate を参照。
	DestinationKeys() []string
}

// registry はチャネルレジストリー。init() による自己登録ではなく、意図的に明示的なリテラルを使うことで、「どんなチャネルがあるか」を
// 1か所ですべて確認でき、新規チャネルの追加時には実行時の副作用に頼らず、コンパイル時に漏れを検出できる。
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get は種類に応じたチャネル実装を取得する。
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind は kind がサポートするチャネルの種類かどうかを返す。
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds はサポートするチャネルの種類をすべて辞書順で返す（UI のドロップダウンを安定して表示するため）。
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError は再試行すべきでない配信失敗を示す：認証情報の誤り、送信先による拒否、不正なリクエストボディなど。
// 再試行は一時的な障害（ネットワークの揺らぎ、レート制限、相手の 5xx）にのみ意味がある。永続的な失敗にバックオフを繰り返しても
// 成功せず、真のエラーが再試行ログに埋もれてしまう。
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent は err を永続的な失敗としてマークする。err が nil なら nil を返し、
// `return Permanent(someCheck())` と書きやすくする。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent は err チェーンに永続的な失敗のマークがあるかを返す。
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 設定読み取りhelper ----
//
// チャネル設定はデータベースの JSONB 列から取得し、encoding/json でデシリアライズすると map[string]any になる。
// 数値はすべて float64、配列は []any。以下の helper はこの変換を統一し、ユーザーが
// UI で空欄にしたために生じた型の違い（ポートを文字列で入力した場合など）も許容する。

// cfgString は文字列の設定項目を取得し、前後の空白をすべて除去する。Web フォームからのコピー＆ペーストで混入しやすいため。
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt は整数の設定項目を取得し、float64（JSON のデフォルト）と文字列の2種類の入力に対応する。
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool はブール値の設定項目を取得し、文字列 "true"/"1" にも対応する。
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings は文字列配列の設定項目を取得し、空白を自動除去して空文字列を破棄する。
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 単一の値のフォーム送信を容易にするため、単一の文字列も受け入れる。
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap は文字列マップの設定項目（カスタム HTTP ヘッダーなど）を取得し、キーと値の空白を除去して空のキーを破棄する。
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
