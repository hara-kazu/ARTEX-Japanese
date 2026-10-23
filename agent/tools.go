package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 上流：この意図の派生元ノード（複数の事実から共同で1つの意図を生成可能）
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 下流：この意図が生成した事実/検出結果
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "人が N 個の目標を追加：…" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "人が N 件の戦略ヒントを追加：…"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "事実1 資産25 脆弱性0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("事実%d 資産%d 脆弱性%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own範囲定義 keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/カスタム agent/サイド質問). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " にはタスクのコンテキスト（探索グラフ）が必要です。現在の agent はタスク内で動作していないため、タスクの探索グラフを取得できず、このツールは使用できません。タスク内で使用するか、task_id を指定するタスク横断の読み取りツール（get_task_node_detail / list_task_findings / get_task_graph など）を使用してください。"), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"（探索経路グラフ）探索状況の要約：資産数、エンドポイントのないサイト、frontier、検出結果、hints（人間 / メイン agentからの方針のヒント。インテントの作成時に必ず考慮する）を返します。計画時には最初に呼び出してください。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 人/メイン agent が add_hint でグラフに付けた戦略ヒント；folded in so the
	// planner reads them every round when generating intents (さもないと書くだけで読まない)。
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, _ := t.ts.Frontier(openIntentsCap) // priority DESC, id ASC —— 優先順位が最も高い先頭 N 件；実際の総数は frontier_open を参照
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 新しい順（all は id 降順）；折り畳み済みは除外済みで、出力時に最新 N 件に切り詰める
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// done_intents_total：終了済み意図（done/blocked/exhausted）の総数で、recent_done_intents と
	// 対応する命名——後者は最新ウィンドウに切り詰めた表示。2つのキーを並べれば"表示件数は N/総数"と自明になり、
	// planner が重複排除で"未表示"を"未割当"と扱わないようにし、プロンプト内の追加説明を不要にする。
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	// frontier_open：開いている意図の実際の総数（open_intents は優先順位が最も高い先頭 N 件に切り詰めた表示）。
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["frontier_open"] = len(fr)
	}
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings_total"] = len(vulnNodes)             // 確認済み脆弱性の総数（目標判定で使用）；詳細は finding_list（最新ウィンドウ）を参照
	out["facts"] = len(factNodes)                      // 探索の事実/結論数（否定的結論を含む）
	// findings はタスク内で最も価値の高い成果物 → 概要に最新ウィンドウを含める（≤10 件、vulnNodes は id 降順で新しい順）。
	// planner が毎回目標を判定する際、最近確認した脆弱性を一目で把握できるようにする；全件/過去のものは list_findings で取得。
	// 各件は {id, summary, from_intent?} のみ保持：from_intent はこの脆弱性を生成した意図。
	// evidence/assets/vulnclass/severity/state などは引き続き list_findings / node_detail(id) で取得可能。
	const findingListCap = 10
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この脆弱性を生成した意図
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	// recent_facts：折り畳まれていない事実の最新ウィンドウ（≤N、factNodes は id 降順で新しい順）。既に
	// digest に折り畳まれ、なお cold なもの（hidden）は cold_digests を使い、ここで重複させない。各件は {id, summary, from_intent?,
	// confidence?}；evidence などの詳細は node_detail(id)。過去のものは list_facts でたどる。
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // digest に折り畳まれ、なお cold —— cold_digests を参照
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この事実を生成した意図
		}
		// confidence を概要に含め、どの結論が inferred に過ぎないかを計画担当が一目で把握できるようにする（特に否定的結論を
		// 確定事項としない）；evidence は長いので、node_detail(id) に委ねる。
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	// recent_done_intents：折り畳まれていない終了済み意図の最新ウィンドウ（≤N、recentDone は id 降順）。
	// 過去のものは done_intents_total の件数 + node_detail(id) を参照。
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: 折り畳んだ cold 領域の digest body を、最新メンバー時刻の降順で先頭 N 件取得；切り詰められた古い digest は
	// id のみ渡す（引き続き expand_digest で展開可能）ことで、cold 領域の唯一の出口が無制限に長くなることを防ぐ。
	const coldDigestsCap = 15
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] —— body を直接読む (§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 切り詰められた古い digest の id；expand_digest(id) で展開
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage：大まかな資産テスト被覆率の参考——範囲(task_scope)内の資産のうち、fact で触れたものの
	// 割合 + by_type(型ごとの 合計/テスト済み)。未テストの具体的な資産は agent が必要に応じて list_untested_assets を呼んで判断する。タスクコンテキストのみ。
	// 資産被覆率機能が無効な場合(coverageDisabled)：host_count(対象ホスト数の認識情報)のみ保持し、
	// denominator/tested/pct/by_type/note などの被覆率指標を捨て、コンテキストの汚染と、既に非表示の
	// add_task_scope/list_untested_assets の誘導を防ぐ。
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage は資産のテストカバレッジ（エンドポイントなど各種の関連資産を含む）の概算で、参考値です。現在のタスクと直接関連するタスクの scope、事実のアンカーを含み、関連する scope は読み取り専用です。コンテナー型の資産や大量の列挙によって値が低くなるため、この値だけでテスト完了と判断しないでください。add_task_scope でこのタスクの範囲を追加し、list_untested_assets で未テストの資産を確認できます【通常は list_untested_assets を呼び出さず、タスクに沿って進めてください】；"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "範囲にアンカー未設定"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// ホスト総数のみを渡し、host 一覧を graph_overview に展開しない（大規模範囲のタスクでは毎回
				// 大量の文字列を繰り返し持ち、計画判断での価値は限定的）；具体的なホストは必要に応じて list_assets で検索。
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 48_000
	relatedOverviewMaxTextPerSource    = 8_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 切り詰められた古い digest の id；expand_digest(id) で展開
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "このタスクと直接関連するタスクの【確認済みの脆弱性】を一覧表示します（簡潔な形式：id + task_id + intent_id + vulnclass + severity + 要約 + 状態）。関連タスクの項目には source_task_id / inherited=true が付き、読み取り専用です。脆弱性だけを含みます。通常の探索事実には list_facts、詳細には node_detail(id) を使用してください。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> 生成元の intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "このタスクと直接関連するタスクの【探索事実 / 結論】を、新しい順にページ分割して一覧表示します（簡潔な形式：id + 要約 + 状態。長い要約は切り詰めます。全文は node_detail(id) で取得）。引数はすべて任意です：limit（既定20件、最大100件）、before（カーソル。前のページが返す next_before を渡すと、より古いページを取得。省略 / 0 は最新ページ）、q（要約のキーワードによる絞り込み）。返り値は {facts, total, has_more, next_before}。total は絞り込み後の総件数で、has_more=true の場合は next_before で次のページを取得します。関連タスクの項目には source_task_id / inherited=true が付き、読み取り専用です。脆弱性は list_findings で確認してください。",
		obj(map[string]any{
			"limit":  intp("返す件数。既定20件、最大100件"),
			"before": intp("ページ移動のカーソル。この値より小さい id の古い事実だけを返します。省略または0は最新ページ"),
			"q":      str("事実の要約をキーワードで絞り込みます（大文字と小文字を区別しない）。省略時は絞り込みなし"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // これを返して次ページ(より古いもの)を取得
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "このタスクまたは直接関連するタスクの【探索グラフのノード】の完全な内容を id で取得します。継承ノードには source_task_id / inherited=true が付き、読み取り専用です。指定できるのは list_facts / list_findings / graph_overview が返す探索ノードの id だけです。資産には list_assets / asset_neighbors を使用してください。",
		obj(map[string]any{"id": idp("探索グラフのノード id（資産 id ではありません）")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id は必須です"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("探索ノード %d が見つかりません。資産を調べる場合は list_assets / asset_neighbors を使用してください（資産と探索ノードは id の体系が異なるため、資産 id を node_detail に渡すことはできません）。", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem は add_intent の一括/単一処理での探索方向1件。
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent は意図ノードを1つ作り、上流の系譜に接続して id を返す。
// 制約：意図は確認済み知識にのみ固定可能——各 parent_id は既存の fact/finding
// ノードであること（別の意図/目標/ヒントには付けられない）。最上位の新しい方向は parent_ids を空にし、origin fact への接続にフォールバック。
// これにより"各意図は fact ノードに接続し、根拠のない計画でなく検出駆動である"ことを作成経路で強制する。
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary は空にできません")
	}
	// 先にアンカーを検証（ノード作成前に行い、不正なアンカーによる孤立意図を防ぐ）。
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d はこのタスクまたは直接関連するタスクに存在しません。parent_ids には既存の【事実（fact）/ 検出結果（finding）】ノードの id が必要です。最上位の完全に新しい方向では parent_ids を空にしてください", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d は %q ノードであり、インテントのアンカーには使用できません。インテントは確認済みの【事実（fact）/ 検出結果（finding）】だけに関連付けられ、インテント / 目標 / ヒントには関連付けられません。最上位の完全に新しい方向では parent_ids を空にしてください", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	// 資産遮断：意図に紐付く資産がシステムの資産遮断ルールに一致した場合、その意図の割当を禁止する。
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, fmt.Errorf("資産のブロック検証に失敗しました：%w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "インテント「%s」に関連付けた資産がテスト範囲の検証を通過しませんでした。関連資産へのテストを停止してください：", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【探索方向】を作成して frontier へ保存し、探索経路に関連付けます。インテントは固定の種類ではなく、自由に考える探索方向です。探索 / 検証 / 悪用する内容を summary の1文で自由に説明してください。\n"+
		"★一括登録を優先：このラウンドで選んだ複数の新しい方向を intents 配列に入れ、1回で登録します（個別に呼び出すより往復回数を減らせます）。返される ids 配列は intents と同じ要素数・順序です（失敗した項目は id=0。詳細は errors）。1件だけなら intents を省略し、最上位の summary を直接指定してください。",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【こちらを優先】追加する探索方向の配列。順番に処理します。各要素は下の最上位フィールド（summary / asset_ids / parent_ids / priority）と同じフィールドを持ちます。返される ids は、この配列と同じ要素数・順序です。", "items": map[string]any{"type": "object"}},
			"summary":    str("[1件登録] 探索方向を1文で説明：行うこと + 理由。方向を明確に記述すればよく、資産 id は必須ではありません。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "この方向でテスト / 攻撃する【対象資産の id】（**できるだけ指定**。0件 / 1件 / 複数件。list_assets が返す資産 id であり、探索ノードの id ではありません）。探索対象の資産（サイト / インターフェース / パラメーター / ホストなど）を示します。具体的な資産を扱う場合は必ず指定してください。対象を構造化して示し、対応範囲の重複排除と、インテントの資産経路への関連付けに使います。全体的な調査だけで、具体的な対象資産が本当にない場合に限り空欄にしてください。"},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "上流のアンカー id（任意。0件 / 1件 / 複数件）。この方向を導いた【確認済みの事実（fact）/ 検出結果（finding）】を指定します。**既存の fact / finding ノードの id だけを指定でき、インテント / 目標 / ヒントは指定できません**。インテントは確認済みの知識に結び付け、検出結果を基に計画し、根拠なく作成しないでください。複数の事実から新しいインテントを導いた場合はすべて指定します。最上位の完全に新しい調査方向なら空欄にしてください（タスクの起点である origin fact に自動で関連付けます）。"},
			"priority":   intp("優先度0～10。既定5"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 単一モード：最上位の summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 人がメイン agent 経由で意図を直接投入 → タスクが done（open 目標なしの goalless 分岐）なら、
			// running に戻し、worker がその意図を取得して実行できるようにする。resumeTask はメイン agent の Chat のみ接続
			// (SetResumeTask)；planner の ToolSet は nil なので、planner 自身の add_intent 呼び出しではこの部分は
			// no-op となり、通常の意図生成に影響しない。意図ノードは既に上で作成済み(open)なので、再開時に枯渇と誤判定されない。
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 単一：元の戻り値を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "このタスクの目標ノードと状態（open / met）を一覧表示し、達成状況の判定に使います。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "検出結果 / 事実が目標の達成を証明していると判断した場合に呼び出します。証拠ノードを目標ノードへ関連付け、目標を met にします。",
		obj(map[string]any{
			"goal_id":     idp("目標ノードの id"),
			"evidence_id": idp("目標の達成を証明する検出結果 / 事実ノードの id"),
			"reason":      str("この証拠が目標の達成を示す理由"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id と evidence_id は必須です"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id はこのタスクの目標ノードである必要があります（関連タスクの目標は読み取り専用です）"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id はこのタスクまたは直接関連するタスクの事実 / 脆弱性ノードである必要があります"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// 目標を1つ met にするたびに、このタスクの【すべての目標】が met かを確認し、そうなら自動で
			// タスク完了（GoalMet を設定）と判定し、モデルによる明示的な goal_met 呼び出しに依存しない。
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("%d件すべての目標が met になりました（最後のトリガーは goal %d）", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met；このタスクのすべての目標が達成されたため、タスクを自動で完了と判定しました", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【タスク全体を即座に終了】タスクの【すべての目標が実際に達成され、全体が完了した】と確認した場合だけ呼び出してください（タスク【全体】の完了が必要です。目標 / flag / 脆弱性のいずれか1つだけの達成では【不十分】です。その場合は prove_goal で該当目標を達成済みにするだけにしてください）。⚠️「今回の計画ラウンドを終了する」ためのツールではありません。追加する新しいインテントがない場合や、worker の成果を待っている場合は、【このツールを呼び出さず、そのラウンドを終了してください】（インテント0件は正常な結果です）。通常の判定では prove_goal による目標ごとの証明を優先します。goal_met は個別の証明を経ずに、タスク全体を直接終了する手段です。",
		obj(map[string]any{"reason": str("達成した理由。実際の目標達成を示す証拠が必要であり、「このラウンドに新しい方向がない」など、ラウンドを終了する理由ではいけません")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "確認済みの脆弱性を記録し、evidence にコマンド出力やログなどの検証可能な証拠を指定します。タスクの実行中は現在の intent_id を渡してください。返される finding_id は独立した脆弱性レコードの ID、finding_node_id は探索ノードの ID です（最初の行にもそのノード番号を保持します）。", obj(map[string]any{
		"vulnclass": str("脆弱性の種類"), "name": str("脆弱性の名前"), "severity": str("critical|high|medium|low"), "summary": str("検出結果の要約"),
		"intent_id": idp("現在のタスクのインテント id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "影響を受ける資産の id"},
		"evidence":         str("証拠 / PoC のテキスト"),
		"evidence_hint_id": idp("任意。この脆弱性に対応する、このタスク内のヒントノードの ID。構造化された traffic_refs を自動で引き継ぎます。継承されたヒントや、別の脆弱性のヒントは参照できません"),
		"traffic_refs": map[string]any{"type": "array", "description": "任意。HTTP / HTTPS の脆弱性では、先に記録を検索し、リクエスト / レスポンスが脆弱性の結論を裏付けていることを1件ずつ確認してから、再現の順序に従って実在する ID を記入してください。TCP など HTTP 以外の脆弱性、未収集の場合、正確な記録が見つからない場合は、省略または [] として構いません。報告は妨げられません。evidence に理由と、別の検証可能な証拠を記載できます。ID の推測、ドメイン / 時刻からの関連付けの推定、通信記録を補うことだけを目的とした再調査はしないでください。用途：baseline は正常時の比較対象、proof は脆弱性の証明、verification は追加検証、supporting は補助証拠。",
			"items": obj(map[string]any{"traffic_id": str("traffic_search が返す実在する通信記録の ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("この通信記録が脆弱性の結論をどのように裏付けるか")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding にはタスクのコンテキストが必要です。プラットフォームのチャットでは add_task_hint で対応タスクに脆弱性を引き継ぎ、ヒントに既存の traffic_refs を含め、タスク Agent に登録を委ねてください。登録済みの脆弱性には bind_finding_traffic で関連付けを追加できます。"), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "無効化済み、ページで手動関連付け可能" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id には有効なヒントノードの ID が必要です。引き継ぎのヒントがない場合は省略してください"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("トラフィック証拠ストアを利用できないため、脆弱性を登録していません"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "脆弱性を保存しましたが、トラフィックは関連付けていません。TCP / 通信記録がない場合は通常どおり続行できます。確認済みの HTTP トラフィックがすでにある場合は、利用可能な bind_finding_traffic または脆弱性ページで関連付けを追加し、証拠の引き継ぎを完了してください。脆弱性を重複して作成しないでください。"
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "脆弱性を保存しました。Agent によるトラフィックの自動関連付けは無効です。ページで手動で関連付けられます。"
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem は record_fact の一括/単一処理での事実1件。
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 結論を支え、事後照合しやすい1行の重要な証拠（コマンド+重要な出力行）
	Confidence string            `json:"confidence"` // observed（直接観測）| inferred（現象から推論）
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact は fact ノード1つを書き込み、意図に接続する（intent→yields→fact）。defaultIntent は
// 一括処理のデフォルト意図（この項目に intent_id がないときに使用）。
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary は空にできません")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id はこのタスクのインテントである必要があります（関連タスクのインテントは読み取り専用です）")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "探索の【事実 / 結論】を探索グラフに保存し、その結果を生んだインテント（intent_id）に関連付けます。フィンガープリント / 列挙などの【肯定的な結論】と、「ポートが閉じている」「パラメーターに注入できない」「ログインの入口が見つからない」などの【否定的な結論】を含む探索結果の記録に使用します。\n"+
		"⚠️1回の探索で得た複数の観察は、分割せず【1件の事実に集約】してください。まとめられるものはできるだけ1件にします。summary は今回の結論を要約する1文、detail は関連する詳細（複数の具体的な項目を含めてもよい）です。例：フィンガープリントを調べるインテント → 1件の事実 {summary:'X サイトの技術構成と応答の特徴を特定した', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'}。ステータスコード、フィンガープリント、タイトルを別々に記録しないでください。通常は1インテントから1件の事実だけを作成します。細分化するとグラフが際限なく膨らみます。\n"+
		"★facts 配列は、【互いに異なる】結論を一度に複数記録するために使います（各項目の intent_id は省略でき、最上位の intent_id を既定値として使用）。返される ids 配列は facts と同じ要素数・順序です。\n"+
		"⚠️ツールの出力で【実際に観察した】結論だけを記録し、想像で補わないでください。不正確な結論がグラフに入るのを防ぐため、evidence と confidence を使用します：\n"+
		"  · evidence=結論を裏付ける重要な証拠を【1行】で記述（コマンド + 最も証明力のある出力1～2行）。**必ず簡潔にしてください**。詳細は detail にあるため、ここへ長い出力を再掲しないでください。\n"+
		"  · confidence=observed（出力で直接観察）| inferred（現象から推測）。\n"+
		"  · **否定的な結論**（注入不可 / ポートが閉じている / 入口が見つからない、など）は、「観察 + 暫定的な解釈」だけを記録してください。実際に観察したことを述べ、その方向を断念するかは計画エージェントが全体を踏まえて判断します。evidence を必ず付け、手段を試し尽くしていない場合や証拠が弱い場合（1回しか調べていない、そう見えるだけ、など）は inferred とし、実際に試し尽くして直接観察できた場合だけ observed としてください。",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【異なる結論が複数ある場合に使用】事実の配列。各要素のフィールドは下の最上位フィールド（summary / detail / evidence / confidence / intent_id / asset_ids）と同じです。intent_id を省略すると最上位の intent_id を使用します。返される ids は、この配列と同じ要素数・順序です。", "items": map[string]any{"type": "object"}},
			"summary":    str("今回の探索の結論を【要約する1文】（detail の概要）"),
			"intent_id":  idp("この事実を生んだインテントの id（割り当てられたインテント。一括登録時には各項目の既定値になります）"),
			"detail":     str("この事実に関する詳細。今回の探索で観察した複数の事実をすべてここに記述してください"),
			"evidence":   str("重要な証拠を【1行】で記述：コマンド + 結論を最もよく証明する出力1～2行。必ず簡潔にし、長い出力を貼り付けないでください（詳細は detail へ）。"),
			"confidence": str("observed（出力で直接観察）| inferred（現象から推測）。否定的な結論でも、実際の確度を正しく指定してください。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関連する資産の id（任意。0件 / 1件 / 複数件）。この事実が関係する資産を指定します"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 単一モード + 一括処理のデフォルト intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // 最上位の intent_id = 一括処理のデフォルト

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 単一：元の戻り値を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint は hint ノード(active/human)を探索グラフに1つ付け、資産に固定可能とし、id を返す。
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent によるトラフィックの自動関連付けが無効のため、traffic_refs を含むヒントを保存していません。システム設定で有効にするか、文章だけを引き継いでください")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text は空にできません")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// planner の起動はここで個別にせず、addHint が全件書き込み後にまとめて1回実行（ヒント文を含む）し、
	// 1回の add_hint の複数ヒントで planner のトリガー行が埋まることを防ぐ。
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal は goal ノード(open)を探索グラフに1つ付け、タスクのルート(origin fact、rel spawns)へ接続する。
// origin は t.worker(省略時 system)を使用:goals 分解器からの書き込みは "goals"、メイン agent の実行時は
// "human" と記録する。planner の起動は setGoals が全件書き込み後にまとめて実行(下記参照)し、ここでは保存のみ担当。
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text は空にできません")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals は【このタスク】に探索目標(goal ノード)を追加する。目標分解器の提出ツールでもあり、メイン
// agent が実行時に目標を補うツールでもある——同じ管理対象ツールで、web で説明/schema の変更と agent ごとの紐付けが可能。
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【このタスク】に探索目標（goal）を追加します。目標は最終的に提供でき、検証できる結果であり、攻撃手順や調査の操作ではありません。\n"+
			"★一括登録を優先：複数の目標を goals 配列に入れ、1回で登録します。返される ids は同じ要素数・順序です（失敗した項目は id=0。詳細は errors）。1件だけなら goals を省略し、最上位の text を直接指定してください。\n"+
			"vulnclass は任意で、対応する脆弱性の種類（SQLi / IDOR など）を指定します。業務ロジックの目標では空欄にしてください。達成状況はシステムが判定して met にします。このツールは追加だけを行います。",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【こちらを優先】追加する目標の配列。順番に処理します。各要素は text（必須。独立して検証できる最終目標を1つ）と vulnclass（任意）を持ちます。返される ids は、この配列と同じ要素数・順序です。", "items": map[string]any{"type": "object"}},
			"text":      str("[1件登録] 独立して検証できる最終目標を1つ"),
			"vulnclass": str("[1件登録] 対応する脆弱性の種類が明確なら指定（SQLi / IDOR など）。業務ロジックの目標では空欄にできます"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals は無効です: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 単一モード:最上位の text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起動(バッチ全体で1回)。notifyGoal を優先:1回の set_goals で「人が
				// N 個の目標を追加:…」のトリガー1件を記録し、個別に埋めない;分解器/worker にこのコールバックがなければ → 単純な notify に戻す(分解器の
				// round-0 は notify も未接続なので操作なし。この時点では planner が未起動のため)。
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// メイン agent が実行時に目標追加 → 完了/一時停止したタスクを running に戻して続行(終端ゲートは
				// 通常の notify を破棄するので、明示的な再開が必要)。このコールバックは mainagent のみ接続し、分解器/worker は nil。
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 単一:元の戻り値を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint は操作制約1件を task_constraints に保存する。origin は t.worker(省略時 system)を使用:
// 分解器は "goals"、メイン agent は "human" を書き込む。
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text は空にできません")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // デフォルトで禁止と扱い、型の未指定時は保守的にする
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type は allow または deny である必要があります")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints は【このタスク】に操作制約(allow=許可する操作 / deny=禁止する操作)を追加する。目標
// 分解器の round-0 での制約抽出の提出ツールでも、メイン agent が実行時に制約を補うツールでもある——同じ管理対象ツールで、web
// で説明/schema の変更と agent ごとの紐付けが可能。制約は planner/worker のシステムプロンプトに注入して探索境界を制約する。
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【このタスク】に操作上の制約を追加して、探索の境界を定めます。type=allow（許可する操作）または deny（禁止する操作）を指定します。\n"+
			"制約は『許可 / 禁止する操作』の規定（例：『現在のポートだけをテストし、ほかのポートはスキャンしない』『本番データベースへの書き込みを禁止』『受動的な調査だけを許可』）であり、目標や攻撃手順ではありません。\n"+
			"★一括登録を優先：複数の制約を constraints 配列に入れ、1回で登録します。返される ids は同じ要素数・順序です（失敗した項目は id=0。詳細は errors）。1件だけなら constraints を省略し、最上位の text / type を直接指定してください。\n"+
			"タスクの目標 / 説明に【明示された】制約だけを登録し、創作しないでください。種類を判断できない場合は、より保守的な deny を使用します。",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【こちらを優先】追加する制約の配列。順番に処理します。各要素は text（必須。1件の制約）と type（allow|deny）を持ちます。返される ids は、この配列と同じ要素数・順序です。", "items": map[string]any{"type": "object"}},
			"text":        str("[1件登録] 操作上の制約を1件記述"),
			"type":        str("[1件登録] allow（許可）または deny（禁止）。省略時は deny として処理"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints は無効です: ExplorationStore が初期化されていません"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 単一モード:最上位の text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 単一:簡単な戻り値を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "人間 / メイン agentからの方針のヒントを探索グラフに追加します。計画エージェントが次回インテントを作成するときに読み取ります。\n"+
		"★一括登録を優先：複数のヒントを hints 配列に入れ、1回で登録します（個別に呼び出すより往復回数を減らせます）。返される ids 配列は hints と同じ要素数・順序です（失敗した項目は id=0。詳細は errors）。1件だけなら hints を省略し、最上位の text を直接指定してください。",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先】追加するヒントの配列。順番に処理します。各要素のフィールドは下の最上位フィールド（text / asset_ids / traffic_refs）と同じです。返される ids は、この配列と同じ要素数・順序です。", "items": obj(map[string]any{"text": str("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[1件登録] ヒントの内容。例：『認証後のインターフェースを重点的に調べる』"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "アンカーとして関連付ける資産の id（任意。0件 / 1件 / 複数件）"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 単一モード：最上位の text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起動（バッチ全体で1回）。notifyHint を優先：1回の add_hint で「人が
				// N 件の戦略ヒントを追加：…」のトリガー1件を記録し、planner に"今回の起動は新規 hint が原因"と明示し、内容を見せる；
				// このコールバックが未接続なら単純な notify に戻す（bare wake、hint はグラフに含めて自身が読めるようにする）。
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 単一：元の戻り値を維持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "実行中のインテント（work）を1件停止します。方向がずれた探索や無意味な探索を中止するために使います。停止したインテントは stopped となり、自動では再取得されません。最初に get_worker_output で現在の作業を確認してから判断してください。",
		obj(map[string]any{"intent_id": idp("停止するインテントの id（work のハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work は現在利用できません"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクのインテントである必要があります（関連タスクのインテントは読み取り専用です）"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("インテント %d の work に終了シグナルを送信しました", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("X を停止して Y に集中"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "実行中のインテント（work）を1件選び、その場で軌道修正の指示を渡します。実行を中断せず、既存の進捗も失いません。worker は次の操作前に指示を受け取り、それに沿って調整します。「X をやめて Y に集中する」など、【インテント内】の軌道修正に使ってください。方向そのものが誤っている場合は、kill_work で停止してから新しいインテントを追加してください。先に get_worker_output で現在の作業を確認することを推奨します。",
		obj(map[string]any{
			"intent_id": idp("軌道修正するインテントの id（work のハンドル）"),
			"message":   str("worker への軌道修正の指示。中止することと、切り替える先を明確に記述してください"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work は現在利用できません"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクのインテントである必要があります（関連タスクのインテントは読み取り専用です）"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message は必須です"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("インテント %d の work に軌道修正の指示を渡しました（次の手順で反映します）", id)), nil
		})
}

// getWorkerOutput returns a work's final (or中止時点までの) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "このタスクまたは直接関連するタスクのインテント（work）を1件選び、最終的な出力と結論を取得します。関連タスクの結果には source_task_id / inherited=true が付き、読み取り専用です。正常終了時は要約を返し、停止（stopped）/ 異常終了した work では、中断時点の最後の出力を返します。",
		obj(map[string]any{"intent_id": idp("インテントの id（work のハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクまたは直接関連するタスクに属していません"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "（この work にはまだ出力がありません）",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("（この work にはまだ出力がありません）"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"インテント（work）を1件選び、【実行過程】を確認します（最終結論だけを返す get_worker_output とは異なります）。使い方は3通りです：\n"+
			"① intent_id だけを指定 → work の各ステップの要約を順番に返します（summary≤100文字、step_id 付き。操作の概要のみで、完全な出力は含みません）。\n"+
			"② intent_id + q → キーワードに一致するステップの要約だけを返します（要約と完全な出力の両方を検索。ただし返すのは summary のみ。内容を見るには③を使用）。\n"+
			"③ intent_id + step_ids → 指定したステップの完全な内容（detail）を返します。1回に最大5件です。超過時は先頭5件だけを返し、取得しなかったものを notice / omitted_step_ids で通知します。\n"+
			"通常は① / ②で気になるステップの step_id を特定し、③で完全な出力を取得します。推論（thinking）のステップは含みません。直接関連するタスクの過去の trace にも対応し、その結果には source_task_id / inherited=true が付き、読み取り専用です。",
		obj(map[string]any{
			"intent_id": idp("インテントの id（work のハンドル）"),
			"q":         str("キーワード。要約 / 完全な出力に一致するステップだけを返します（任意。step_ids とは同時に指定できません）"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "完全な内容を取得する step_id（① / ②の返り値から取得）。1回に最大5件。超過時は先頭5件だけを返し、残りは omitted_step_ids に列挙します"},
			"limit":     intp("要約一覧 / 検索で返す件数の上限（任意）"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須です"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id はこのタスクまたは直接関連するタスクに属していません"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"一回に取得できるステップの全文は最大%d件です。今回は先頭%d件（%v）を返しました。未取得の%d件は %v です。"+
							"これらの内容で十分に特定できる場合、残りのステップの取得は不要です。続きが必要な場合は、これらの step_id でもう一度呼び出してください。",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【大半の情報はシステムがすでに提供しているため、通常は使用を推奨しません】キーワード（q）で【このタスクのほかの work の実行過程】を検索します。worker が観察したものの fact に書き込んでいない情報（パス / token / エラーなど）を探すために使います。"+
			"自分のインテントのステップは自動で除外します（すでに自分のコンテキストにあるため）。"+
			"一致したステップの要約（summary≤100文字）だけを返し、各項目に intent_id が付きます。それを基に get_worker_trace(intent_id, step_ids=[...]) で完全な内容を取得してください。",
		obj(map[string]any{
			"q":     str("キーワード（すべての work のステップの要約 + 完全な出力を検索）"),
			"limit": intp("返す件数の上限。既定100件（任意）"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q は必須です"), nil
			}
			// 呼び出し元自身の意図のステップは除外（worker 自身の trace はコンテキストに既存）。
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【大半の情報はシステムがすでに提供しているため、通常は使用を推奨しません】このタスクの【実行済みの work（インテント）】の索引を一覧表示します。intent_id + 方向を示す一文（summary）+ 状態を返します。"+
			"あなた（worker）は探索グラフを参照できないため、このツールで詳しく確認する価値のある work を探してください。その後 get_worker_trace(intent_id) でステップを確認し、get_worker_trace(intent_id, step_ids=[...]) で詳細を取得してください。"+
			"実行済みのもの（running/done/exhausted/blocked/stopped）だけを一覧表示し、未実行の open は含みません。担当範囲は引き続き割り当てられたインテントだけです。他の work の閲覧は、観察の再利用と重複作業の回避のためだけに行ってください。",
		obj(map[string]any{
			"q":     str("summary のキーワードで絞り込み（任意）"),
			"limit": intp("返す件数の上限。既定50件（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding：計画時の状況判断で自身が脆弱性を確認できた場合、直接登録可能（worker と同じツール）。
		t.addFinding(),
		// list_companies：企業一覧 + scope + 資産数を表示（company_id の取得 / 所属範囲の理解）。
		t.listCompanies(),
		// list_assets：計画時に DSL で全資産 DB を検索（list_untested_assets の"範囲内の未テスト"の視点と組み合わせ、
		// "ドメイン/指紋/ポート/状態コードなどの条件で DB 全体を検索"する能力を補う）。
		t.listAssets(),
		// add_company_scope：計画時にドメイン/IP/CIDR/ICP/キーワードを特定企業の資産範囲に追加可能（一致した資産を自動でその企業に帰属させる）。
		t.addCompanyScope(),
		// add_task_scope：ルートドメイン全体/企業全体/特定サブドメイン/IP をこのタスクのテスト範囲(被覆率の分母)に能動的に追加。
		t.addTaskScope(),
		// list_untested_assets：必要に応じてこのタスクの範囲内の未テスト資産を検索(型+ページ分割)し、追加テストを自ら判断。
		t.listUntestedAssets(),
	}
}
