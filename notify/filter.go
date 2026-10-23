package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter は notification_channels.filter という JSONB 列の契約：チャネルインスタンスのフィルター条件。
// すべてのフィールドは任意で、省略時は「フィルターなし」。不正な設定のフォールバックの意味でもある。ParseFilter を参照。
type Filter struct {
	// MinSeverity は最低レベルの閾値（low/medium/high/critical）で、空なら閾値なし。
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs が空配列なら制限なし。空でなければイベントとの共通要素を要求する。
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude は空ならすべて受け入れ、空でなければ vulnclass がいずれかのキーワードに一致する必要がある。
	// VulnClassExclude はいずれかのキーワードに一致すれば除外する（除外が包含に優先）。
	// 照合は大文字小文字を区別しない部分文字列で行う。正規表現より安全で、ユーザーの正規表現の誤設定でチャネルが黙って無効化されない。
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange はチャネルが脆弱性の状態変更イベントを受信するかを決める（realtime モードでのみ意味がある）。
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter はチャネルのフィルター設定を解析する。
//
// **error を返すことはない。** これは意図的な設計上の選択：フィルター条件の設定が不正な場合はすべてゼロ値の
// Filter（= フィルターなし = すべて一致）に退化させる。脆弱性通知システムでは、**余分に1件プッシュするほうが
// 高リスクの1件を黙って漏らすよりはるかによい**ため。解析失敗を「プッシュしない」にすると、一見設定済みに見えて
// 実際には何もプッシュしないチャネルをユーザーに渡すことになる。これは最悪の失敗モードである。
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 解析失敗時は f のゼロ値を維持し、フィルターしない。
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity は s が有効なレベルの閾値かを返す（空文字列は閾値なしを示す）。
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate はフィルター設定内の**値が制限される**フィールドを検証し、チャネル保存時に呼び出す。
//
// 書き込み時に阻止する必要がある理由：Match の不明な閾値に対する判定は `rank >= 0` で、常に真となる。
// つまり min_severity を1文字誤記すると（"hgih"）、フィルターが**黙って無効化**されて
// 「すべてプッシュ」になる。これは本パッケージの「漏らすより多めにプッシュ」という方針に沿ってはいるが（漏れはない）、
// ユーザーはレベル別にプッシュしているつもりで、実際は全脆弱性をグループに流し込み、
// 誤設定を示す兆候も一切ない。この種の「黙った縮退」は入口で阻止すべきである。
//
// 注意：Validate は**書き込み**経路専用。読み取り経路は ParseFilter の寛容な意味を維持し、
// 既存データの不正な値のためにチャネル全体を読み取れなくしない。
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("最低重大度 %q が無効です。low / medium / high / critical、または空欄で制限なしにしてください", f.MinSeverity)
	}
	return nil
}

// Match は、このフィルター条件を持つチャネルにイベントを配信すべきか判定する。
//
// **error を返すことはない**。理由は ParseFilter と同じ：内部の例外はすべて「一致」として扱う。
// 判定順：イベントの種類 → レベルの閾値 → タスク/アセットの範囲 → 脆弱性の種類のキーワード。
func Match(f Filter, s Snapshot) bool {
	// 状態変更イベントは明示的に有効にしたチャネルのみ受信する。大半のユーザーにとって
	// 「プッシュ」とは「新しい脆弱性の発見」で、すべての状態遷移の逐次記録ではないため、デフォルトは無効。
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 除外優先：包含リストにも一致していても、いずれかの除外キーワードに一致すれば除外する。
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 小さい集合なので線形走査で十分。両側とも「手作業で選ぶ数十件」程度であり、
	// map を構築するコストが効果を上回る。
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold は s が keywords のいずれかのキーワードを含むかを返す（大文字小文字を区別しない）。
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
