package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates は挿入待ちの資産入力項目からドメイン/IP/URL 候補文字列を抽出し、資産遮断の照合に使用する。
// URL の host を分離して分類し、「URL のみ」のサービス/端点資産もドメイン/IP ルールに一致するようにする。
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
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

// assetInputLabel は挿入待ち資産の短い識別情報を返し、遮断の説明メッセージに使用する。
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "（不明）"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"新しく発見した資産を一括登録します。1回で複数の種類を混在させられます（type の列挙値を参照）。\n"+
			"種類ごとの必須フィールド：root_domain→domain、ip→ip（IPv4 / IPv6 アドレスが必要。ホスト名は不可）、subdomain→domain、app→app_name、service（HTTP）→url、service（HTTP 以外）→service_name+port（ip / domain のどちらか1つ以上が必要）、endpoint→url+method。その他のフィールドは各説明を参照してください。\n"+
			"auth / technologies / params は追記して統合（append）し、元の値を上書きしません。\n"+
			"返り値：{results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id はモデルに公開しない：worker の所属 task はプログラムが SetTaskID で正式に設定する(handler を参照)。
			"assets": map[string]any{
				"type":        "array",
				"description": "資産の配列。各要素が1件の資産レコードに対応します",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "資産の種類",
					},
					// root_domain / subdomain
					"domain":      str("ルートドメインまたはサブドメイン（root_domain / subdomain では必須）"),
					"icp":         str("ICP 登録番号（任意）"),
					"record_type": str("DNS レコードの種類：A / AAAA / CNAME / MX など（subdomain で指定可能。任意）"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS レコードの値の一覧（subdomain で指定可能。任意。例：[\"1.2.3.4\",\"2.3.4.5\"]）",
					},
					// ip
					"ip": str("IP アドレス。IPv4 / IPv6 アドレスが必要で、ホスト名は指定できません（ホスト名には type=subdomain の domain フィールドを使用）。ip では必須。service / endpoint でも IP の関連付けに使用できます"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "この IP に関連付けるドメインの一覧（ip で指定可能。任意）",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "開いているポートの一覧（ip で指定可能。任意）",
						"items": obj(map[string]any{
							"port":    intp("ポート番号"),
							"service": str("サービス名。http / ssh / mysql など（任意）"),
						}, "port"),
					},
					// app
					"app_name":    str("アプリ名（app では必須）"),
					"bundle_id":   str("Bundle ID（app で指定可能。任意）"),
					"category":    str("アプリのカテゴリ（任意）"),
					"description": str("アプリの説明（任意）"),
					"app_icp":     str("アプリの ICP 登録番号（任意）"),
					"company_id":  intp("所属企業の id（app で指定可能。任意。app は scope から所属を自動判定できないため、明示的な指定が必要です。id は add_company_scope が返します）"),
					// service (http)
					"url":         str("プロトコルとポートを含む完全な URL（HTTP サービスでは必須。service_type は自動で http に設定）"),
					"status_code": intp("HTTP レスポンスのステータスコード。200 / 301 / 403 / 404 など（任意）"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP レスポンス本文のバイト数（任意）",
					},
					"page_title":   str("ページの <title> の内容（任意）"),
					"favicon_mmh3": str("favicon の MMH3 ハッシュ（任意）"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "フィンガープリント / 技術構成の一覧。例：[\"Nginx\",\"Vue\",\"Bootstrap\"]（任意）",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "発見した認証情報の一覧。各項目に type / username / password などのフィールドを含めます（任意。上書きせず追記）",
						"items":       map[string]any{"type": "object"},
					},
					// service (other、非 HTTP)
					"service_name": str("サービス名。ssh / mysql / redis など（HTTP 以外の service では必須）"),
					"port":         intp("ポート番号（HTTP 以外の service では必須）"),
					// endpoint
					"method": str("HTTP メソッド：GET / POST / PUT / PATCH / DELETE など（endpoint では必須）"),
					"params": map[string]any{
						"type":        "array",
						"description": "リクエストパラメーターの一覧。各項目に location（query / body / header / path）/ name / value / type を含めます（任意。上書きせず追記）",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets は無効です: AssetStore が初期化されていません"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id はプログラムが正式に設定(worker: SetTaskID)し、モデルからは受け付けない——モデルの省略/誤指定により、
			// 資産がタスクに属さない、または誤ったタスクに属することを防ぐ。タスクコンテキストなしの呼び出し元(auto/pentest/chat)は t.taskID=0。
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// 資産ゲートルールを一括で読み込み、読込失敗時は判定を省略（挿入は阻止しない）。
			// 遮断ルール = グローバル ∪ タスク単位の block；許可ルール = タスク単位の allow。
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// 資産ゲート：先に遮断、次に許可を判定し、拒否された資産は挿入禁止（Upsert と後続の副作用をスキップ）。
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("資産 %s：%s。登録は禁止されています", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent が insert_assets で登録"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker のインテント #%d が insert_assets で登録", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// テスト範囲への自動追加(source='auto')：worker が最上位で明示的に挿入したこの項目だけに、その
				// 型に応じた保守的な範囲を追加する;side-effect で派生した資産はここを通らず、範囲を無条件に広げない。taskID=0 では操作なし。
				// 被覆率スイッチとは無関係：task_scope はタスクの範囲境界(list/クエリのフィルター基準)で、
				// 被覆率スイッチは指標計算の分母にするかだけを決め、範囲自体を蓄積するかは決めない。
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"ドメイン / IP / CIDR / ICP 登録番号 / 企業のキーワードを、指定企業の【資産範囲】へ追加します。ドメイン、ネットワーク、ICP で一致した資産は自動で所属を設定します。キーワードは Agent への範囲のヒントとしてだけ提供します。\n"+
			"企業名は一意です。company が存在しなければ作成し、すでに存在する場合は再利用して範囲だけを統合します。\n"+
			"scope は1行に1項目を記述します。ルートドメイン / URL / 単一 IP / CIDR のネットワーク / ICP 登録番号 / 企業のキーワードをシステムが自動判別します。\n"+
			"reason に所属を判断した根拠（whois / 証明書 / ASN など）を必ず指定してください。\n"+
			"制限：TLD だけの指定や広すぎるネットワークは拒否します（IPv4 のプレフィックスは /16～/32、IPv6 は /32～/128 が必要）。無効な行はスキップし、errors に返します。",
		obj(map[string]any{
			"company": str("企業名（存在しなければ作成し、存在する場合は再利用。名前は一意）"),
			"scope":   str("資産範囲。1行に1項目：ドメイン / URL / IP / CIDR / ICP 登録番号 / 企業のキーワード"),
			"reason":  str("所属を判断した根拠（証拠 / 情報源）。必ず記入してください"),
			"logo":    str("企業アイコンの URL（任意。企業を新規作成するときだけ有効）"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope は無効です: CompanyStore が初期化されていません"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company は空にできません"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("企業の作成 / 取得に失敗しました: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"【このタスク】にテスト範囲を追加します。このタスクの許可範囲の境界であり、資産のテストカバレッジの分母にもなります。\n"+
			"対応する kind：company（企業に所属するすべての資産）/ root_domain（全サブドメインを含むルートドメイン全体）/ subdomain（完全一致のサブドメイン1件）/ ip / cidr / icp / keyword。\n"+
			"worker が個別に扱ったホストは、完全一致のサブドメインとしてシステムが【自動で】範囲に追加します。このツールは、ルートドメイン全体 / 企業全体を含めたり、特定のサブドメイン / IP を補ったりして、【意図的に範囲を拡張】するために使用します。\n"+
			"value：company には企業名または id（企業がすでに存在すること）、root_domain / subdomain にはドメイン、ip / cidr には IP またはネットワーク、icp / keyword には登録番号または企業のキーワードを渡します。\n"+
			"監査できるように、reason に根拠を必ず指定してください。複数件は entries 配列で指定します。",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "一括登録：[{kind, value}]。kind は company / root_domain / subdomain / ip / cidr / icp / keyword のいずれか。", "items": map[string]any{"type": "object"}},
			"kind":    str("[1件登録] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[1件登録] 企業名または id / ドメイン / IP / CIDR / ICP / キーワード"),
			"reason":  str("追加の根拠（監査用）。必ず記入してください"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope は無効です: AssetStore が初期化されていません"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope にはタスクのコンテキストが必要です（現在 task がありません）"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 単一モード
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"【このタスクと直接関連するタスク】の範囲内で、まだ事実のアンカーに関連付けられていない資産を検索します（関連する範囲は読み取り専用です。追加テストの要否を自分で判断するための情報であり、判断を代行するものではありません）。\n"+
			"資産の種類による絞り込みは任意です：root_domain / subdomain / service / app / endpoint / ip。\n"+
			"ページ分割：page は1から、page_size の既定値は10件です。返り値は {assets:[{id,type,label}], total, page, page_size}。タスクのコンテキストがある場合だけ使用できます。",
		obj(map[string]any{
			"type":      str("資産の種類による絞り込み（任意）：root_domain / subdomain / service / app / endpoint / ip"),
			"page":      intp("ページ番号。1から開始（既定1）"),
			"page_size": intp("1ページの件数（既定10件）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets は無効です: AssetStore が初期化されていません"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets にはタスクのコンテキストが必要です"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"資産データベースを DSL 式で検索するか、id / ids で直接取得します。ページ分割に対応し、【このタスクと直接関連するタスク】のテスト範囲内の資産だけを返します。\n"+
			"DSL：field=value は部分一致（ILIKE）、field==value は完全一致、field!=value は除外、数値フィールドは > >= < <= に対応、フィールド名のない単語は全文の部分一致検索です。AND / OR で結合し（AND が優先）、括弧でグループ化できます。資産の種類は独立した type 引数で指定し、DSL には記述しないでください。\n"+
			"id / ids を指定しない場合、dsl は空にできません（無条件の全件検索は禁止）。\n"+
			"使用できるフィールド：domain（ルート / サブドメイン / サービスのドメイン）、root_domain、ip、url、page_title、icp、service_name、app_name、method（GET / POST など）、service_type（http|other）、record_type（A / CNAME など）、technology（配列。= は部分一致、== は完全一致）、port / status_code / company_id（整数）。\n"+
			"例：status_code>=400 AND technology=shiro ; (port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL 検索式（構文 / フィールドはツールの説明を参照）。id / ids を指定しない場合は必須で、空にはできません。`),
			"type":   str("資産の種類で絞り込み：root_domain|ip|subdomain|app|service|endpoint（独立したフィールド。dsl と併用可能。type だけでは検索できず、dsl も必要）"),
			"id":     intp("単一の資産 id で直接取得（任意。dsl / type とは同時に指定できません）"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "複数の資産 id で直接取得（任意。dsl / type とは同時に指定できません）"},
			"limit":  intp("返す件数の上限。既定10件（任意）"),
			"offset": intp("ページ分割のオフセット。既定0（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets は無効です: AssetStore が初期化されていません"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id/ids を指定しない場合は dsl を空にできません。無条件での全資産の検索は許可されていないため、検索条件を指定してください"), nil
			}
			if err != nil {
				return actool.Errorf("DSL エラー: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies (企業) with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"資産データベース内の【企業】と資産範囲（scope）、所属する資産数を一覧表示します。企業の確認と、"+
			"company_id の取得に使用します（insert_assets で app を関連付ける場合や、list_assets で company_id による絞り込みを行う場合に使用）。"+
			"任意の search で企業名の部分一致検索ができます（大文字と小文字を区別しない）。空欄なら全件を返します。",
		obj(map[string]any{
			"search": str("企業名の部分一致で絞り込み（任意。大文字と小文字を区別しない）。空欄なら全件を返します"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies は無効です: CompanyStore が初期化されていません"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("企業の検索に失敗しました: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings を保持：脆弱性報告前にこのタスクの確認済み脆弱性を検索し、同じ脆弱性の重複報告を防ぐ。
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope は worker に与えない：企業の資産範囲の定義は計画/主制御/Auto の責務で、worker は探索の実行のみを担う。
		t.insertAssets(), t.listAssets(),
		// work をまたいで参照：worker も他の work の観測を再利用し、重複作業を避けられる。
		// search_all_worker_traces：intent_id を事前に知らなくても、キーワードで全体から一致ステップを取得；
		// get_worker_trace：特定の work を絞り込んだ後、ステップ一覧/その場で検索/完全な内容を取得。
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail：worker は intent_id/ノード id を取得後、そのノードの全詳細を照会可能（上記の参照と併用）。
		t.nodeDetail(),
		// 以下のツールは引き続き worker に【与えず】、planner/main 専用とする（コンテキスト読込や work 間の振り返りは計画の責務で、
		// worker は意図1件の実行と書き戻しのみ担当）：list_facts / list_companies / list_worker_traces。
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work：人が実行中の意図(work)に修正指示をリアルタイム注入可能（中断せず、進捗を失わない）。
		t.steerWorkTool(),
		// set_goals：人が実行時にこのタスクへ新たな最終目標を追加可能（計画担当はそれに基づき達成有無を再判定）。
		t.setGoals(),
		// set_constraints：人が実行時にこのタスクの操作制約（allow/deny）を追加/変更し、planner/worker の探索境界を制約できる。
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets：必要に応じてこのタスクの範囲内の未テスト資産を検索(型+ページ分割)し、追加テストを自ら判断。
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
