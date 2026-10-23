package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// このファイルは「組み込みツール」を純粋なコードから、列挙可能で DB による上書きが可能なカタログにする：
//   - BuiltinToolSeeds()：3つの実行 agent の組み込みツール群を seed 記録（key +
//     説明 + 引数 schema + デフォルトで紐付ける agent）へ展開し、サーバーが起動時に tools テーブルへ冪等に seed する。
//   - ToolResolve フック：実行時に DB の tools 行に基づき、組み立て済みツールへ「agent ごとのフィルター +
//     説明/schema の上書き + 引数デフォルト値の注入」を行う。key/handler はコード層に残り、DB は「説明文とデフォルト値」のみ変更する。
// handler（Call の動作）は常にコード由来で、DB では変更できず、モデルに見える説明と省略時の入力引数だけを変更できる。

// ToolSeed は組み込みツールの seed 可能なスナップショット：key は CoreTool.Name()（handler と固定結合し、
// UI は読み取り専用）、Desc/Schema はコード内のツール定義から取得し、Agents はコードでデフォルト付与する agent。
type ToolSeed struct {
	Key    string         // = CoreTool.Name()、主キー、変更不可
	Desc   string         // 最上位の説明（UI で上書き可能）
	Schema map[string]any // 引数 JSON-Schema（構造は読み取り専用、description/default は UI で変更可能）
	Agents []string       // デフォルトで紐付ける agent key（worker/planner/mainagent）
}

// builtinToolsByAgent は「読み取り専用の空の外枠」ToolSet（nil stores）で各実行 agent の
// ドメインツール群を構築する。ツール構築関数は Spec にクロージャを格納するだけで、構築時に store を参照解除しないため nil でも安全——
// ここではこれらのツールの Name()/Description()/InputSchema() を読むだけで、Call は決して行わない。
//
// SDK の汎用ツール actool.DefaultTools()（Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash）は意図的に【含めない】：各 agent が固定で持ち、「誰に紐付けるか」の選択がなく、説明の大半は Prompt() に
// ある（本テーブルは Description() だけ上書きするため、半端な上書きで誤解を招く）。seed なし → DB 行なし → ToolResolve は
// そのまま通し、上書きせず、従来の動作を維持する。artex 自身のドメインツールのみテーブルで管理可能。
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals（目標分解器）はデフォルトで set_goals + set_constraints を紐付け、分解した目標と
		// 抽出した操作制約を保存する。mainagent と同じ管理対象ツールを共有し、web で説明/schema の変更と agent ごとの選択が可能。
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto はデフォルトで脆弱性報告 + 資産管理ツールを紐付け、他のドメインツールは UI で必要に応じて選択可能。
		// 新規 DB はこの seed で書き込み、旧 DB は seedAutoDefaultBindings で移行する。
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（独立侵入テスト agent）のデフォルト紐付け：資産検索 / 資産挿入 / 脆弱性報告 / 脆弱性検索 / 企業検索。
		// 新規 DB はこの seed で書き込み、旧 DB は seedPentestDefaultBindings で移行する。
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound：これらの system ツールは通常どおりカタログに入る（web で表示され、agent ごとに手動選択可能）が、
// デフォルトでは【どの agent にも紐付けない】——ToolResolve は空の紐付けのツールを全 agent で破棄し、明示的な opt-in が必要。
// それでも agent の base ツール群に残す理由(goal_met が PlannerTools にあるなど):第一に seed が
// 構築して desc/schema を取得でき、第二にユーザーが手動で紐付け直した後も実行時の base に存在し、ToolResolve が保持できるため。
//
// goal_met：個別の prove_goal を迂回し、全体から直接【タスク全体の完了】を宣言するため、影響が大きく誤判定リスクがあり、さらに
// 「prove_goal が最後の目標をマーク → 自動終了」と重複するので、デフォルトではどの agent にも与えず、必要時に手動で紐付ける。
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds は各 agent の組み込みツール群を重複排除し、seed 一覧に統合する：同名ツール（list_assets が
// 複数の agent にある場合など）は1件に統合し、Agents は和集合を取る；defaultUnbound のツールは紐付けを強制的に空にする。
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // カタログに入り、手動紐付け可能だが、デフォルトではどの agent にも与えない（他のツールと同様、null でなく [] を保存）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 省略時の入力引数 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — onlyデフォルト値 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
