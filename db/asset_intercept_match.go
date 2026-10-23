package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 資産遮断ルールの照合/実行層。asset_intercept.go はルールの保存のみを担当し、ここでは
// 「対象資産」のドメイン/IP/URL を有効なルールと照合する。agent ツール（add_intent、
// insert_assets）が意図の送信 / 資産の挿入前に呼び出し、一致すれば拒否する。

// AssetInterceptKindLabel は kind の中国語ラベルを返し、agent への説明メッセージに使用する。
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "ドメイン(完全一致)"
	case "exact_ip":
		return "IP(完全一致)"
	case "exact_url":
		return "URL(完全一致)"
	case "fuzzy_domain":
		return "ドメイン(部分一致)"
	case "fuzzy_ip":
		return "IP(部分一致)"
	case "fuzzy_url":
		return "URL(部分一致)"
	case "cidr":
		return "CIDR ネットワーク範囲"
	}
	return kind
}

// Reason は読みやすい一致理由を1つ返す。例：資産遮断ルールに一致 [ドメイン(あいまい): .gov.cn]（備考）。
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("資産のブロックルールに一致 [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne は有効な単一ルールが指定のドメイン/IP/URL 候補文字列に一致するか判定し、一致した具体値を返す。
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules は指定のドメイン/IP/URL 候補文字列に一致する最初の有効なルールと、一致した具体値を返す。
// insert_assets が元の入力（まだ保存されていない assetInputItem）を照合する際に使用する。
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates は保存済み資産から遮断照合用のドメイン/IP/URL 候補文字列を抽出する。
// URL の host を分離して分類し、「URL のみ」のサービス資産もドメイン/IP ルールに一致するようにする。
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel は資産の短い識別情報を返し、agent への説明メッセージに使用する。
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("資産#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule はルール集合に有効なルールが1つでも存在するか判定する。
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision は候補文字列群に対する「遮断を先に判定し、その後に許可する」ゲートの判定結果。
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 拒否理由（資産識別情報を含まない）；Allowed=true のとき空
}

// EvaluateAssetGate はタスク単位のゲート判定を実行する：
//  1. 有効な blockRules のいずれかに一致 → 拒否（遮断理由）。
//  2. それ以外で allowRules に有効項目があり、どれにも一致しない → 拒否（許可範囲外）。
//  3. それ以外は通過を許可する。
//
// allowRules が空/有効項目なしの場合、許可ゲートは作用しない（許可リストを有効にせず、すべて通過を許可する）。
// 「許可ルール未設定」によって全資産が遮断されることを避ける。
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "タスクの許可リストの範囲外のため、テストできません"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit はゲートに拒否された資産（遮断に一致、または許可範囲外）を表す。
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 読みやすい理由
}

// Describe は読みやすい説明を1つ返す：資産情報 + 理由。
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules は *DB の同名メソッドにそのまま渡し、AssetStore だけを保持する呼び出し元
// （agent ツールなど）もルールを読み取れるようにする。
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept は id ごとに資産を読み込み、「遮断を先に判定し、その後に許可する」ゲート判定を順次実行し、すべての
// 拒否された資産を返す。遮断ルール = グローバル ∪ タスク単位の block；許可ルール = タスク単位の allow（このタスクのみ）。
// id がなければ即座に返す。グローバルな GetByIDs（タスク範囲のフィルターを受けない）を使用し、scope によって遮断が弱まらないようにする。
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// 遮断ルールも有効な許可ルールもない → 判定不要、すべて通過を許可する。
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
