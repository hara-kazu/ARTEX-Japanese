package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【你的规划待办（跨唤醒保留，上一轮你写的）】：\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("据此推进：只对【前置步骤已完成 / 其依赖的 fact 已存在】的下一步派意图；用 TodoWrite 更新清单（把已被 fact 满足的步骤标 completed）。不要重复派已在清单里 pending/in_progress 的步骤。")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 摘要).
//	"goal"    — the human (via 主 agent 的 set_goals) added one OR MORE goals in a
//	            single call (Goals = 本次新增的目标文本，1+ 条；set_goals 支持批量).
//	"goal_deleted" — the human deleted a goal from 总览的目标管理 (Detail = 被删目标文本).
//	"goal_edited"  — the human edited a goal from 总览的目标管理 (OldGoal→NewGoal 文本).
//	"cancelled" — the human deleted intent IntentID (Detail = 删除原因). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 专用：删除前捕获的意图摘要（真删除后节点已不存在，无法再查）
	Goals    []string // Kind=="goal" 专用：本次 set_goals 新增的目标文本（1 条或多条）
	OldGoal  string   // Kind=="goal_edited" 专用：修改前的目标文本
	NewGoal  string   // Kind=="goal_edited" 专用：修改后的目标文本
	Hints    []string // Kind=="hint" 专用：本次 add_hint 新增的提示文本（1 条或多条）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【本次触发本轮的实际变动（先看这里，再决定是否补方向）】：")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了一个目标：%s —— 新的待达成目标，请据此补充探索方向（若尚无对应意图）。", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了 %d 个目标：%s —— 均为新的待达成目标，请逐一为尚无对应意图的目标补充探索方向。", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了一条战略提示：%s —— 已挂到探索图上，请据此调整/补充探索方向（若尚无对应意图）。", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了 %d 条战略提示：%s —— 均已挂到探索图上，请逐一据此调整/补充探索方向。", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人删除了该目标：%s —— 该目标已移除，请据此重判剩余目标/方向（不必再为它派意图）。", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人修改了目标，由「%s」变为「%s」—— 请据新目标调整探索方向（原方向若已不适用请停派）。", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 意图 #%d（%s）的 worker 报告了一个 finding：%s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意图内容优先用删除时捕获的 Summary（真删除后节点已不存在，intentSummary 查不到）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 意图 #%d 由用户删除，意图内容是：%s、删除原因是：%s。该意图已删除（不再执行）；请据此重新规划。", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 意图 #%d（%s）的 worker 结束，输出结论：%s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("；本意图新产生的事实 id：%s ", fids))
			}
		}
	}
	b.WriteString("\n（完整细节可 node_detail / get_worker_output / list_findings 再查。）")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(取输出失败)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(该 work 尚无输出记录)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …（已截断，完整见 get_worker_output）"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【本轮态势（graph_overview 预取，等同你调用该工具的返回；需要细节再按需调 node_detail/list_facts 等）】：\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中间产物输出规约
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `あなたはサイバーセキュリティ基盤上で、許可を得たペネトレーションテストの「計画」を担当します。グラフが変わるたびに起動されます。状況を把握し、目標の達成を判定し、**まだ扱われていない新しい方向が実際にある場合だけ**探索インテントを追加してください。あなたは計画担当であり、実行担当ではありません。このラウンドの成果は【インテントの作成 / 具体化】または【目標の達成判定】に限り、plan の中で実作業を済ませてはいけません。

タスクの目標：{{.Goal}}

**このラウンドで作成するインテントの数（最初に判断すること）**：
- **必須条件（最優先）**：【目標が未達成】かつ【open または running のインテントが1件もない】場合（frontier_open=0 かつ running_intents が空）、このラウンドで目標へ進むインテントを少なくとも1件【必ず】作成してください。実行中の work も待機中の方向もない状態で0件にすると、タスクが停止してしまいます。既知の方向が recent_done にしかなくても、以下の done / exhausted / blocked の判定に従って新しい方向を追加するか、継続のインテントを追加してください。
- 上の必須条件に該当しなければ、**インテントが0件でも正常ですが、正当な理由が必要です**。「少ない方が安定する」という理由で0件を既定にしないでください。正当な理由は、①**すでに対応中**：考えた方向はすべて open / running のインテントが扱っている（既存インテントの言い換えによる重複作成は重大な誤り）、②**依存する結果を待機中**：次の作業は実行中の work の成果に依存し、その成果がまだない（無理に追加すると後続が前提を得られず空回りするため、グラフ更新による次回の起動を待って追加する）、のいずれかです。
- 逆に、【未対応で、実行中の work に依存しない】新しい方向がある場合や、目標が未達成で範囲内に未テストの領域が残る場合は、インテントを追加してください。作業を省くために0件を既定にしてはいけません。

**起動されるたびの判断手順**：

1. **状況の全体像をこのプロンプトの下に添付しています**（graph_overview の返り値なので、再度呼び出す必要はありません）：task（元のタイトル + 目標 / ルートノード）、資産数、goals と状態、open / running / recent_done のインテント、sites_without_endpoints（エンドポイントがないサイトで、未探索の方向を示します）、facts（探索で得た事実の数。脆弱性とは別の種類）、recent_facts（{id,summary,confidence?}）。
   - **範囲**：探索ノード（goals / インテント / facts / findings）はこのタスクだけを含みます。**資産グラフは全体で共有**します（複数のタスクで共通のグラフを使用し、資産数は全体の範囲内の数であり、このタスク専有の数ではありません）。このタスクと関係のない資産は無視してください。
   - **派生関係**：各インテントには parents（派生元の事実 / インテント）と yields（そのインテントが生成した事実 / 検出結果）があり、recent_facts の各項目には from_intent があります。これを使い、どの方向からどの事実が得られたか、それらを組み合わせて新しい方向を導けるかを判断してください。
   - **否定的な観察 / 疑わしい観察**（recent_facts の「ポートが閉じている / 注入できない」など）は worker の観察であり、確定した結論ではありません。採用する前に node_detail(id) で evidence を確認してください。evidence が確かで、confidence=observed であり、手段を試し尽くしている場合だけ、その方向は当面進めないと判断します。evidence がない、「そう見える / 1回しか試していない」、または confidence=inferred の場合は【未解明】として扱い、範囲内でほかのインテントも扱っていなければ、原則として再確認のインテントを1件追加し、観察を裏付けるか覆してください（**同じ否定的な方向の再確認は最大1回**。再確認後も否定的で証拠が妥当なら、その結論を尊重し、それ以上追加しません）。
   - **詳細が必要な場合だけ呼び出すツール**：list_facts（ページ分割、新しい順、既定20件。q で絞り込み、before でページ移動。total / has_more 付き）、list_findings（すべての脆弱性）、node_detail(id)（完全な証拠 / 詳細。一覧 / recent_facts は要約のみ）、list_assets（pull：必要な分だけ取得。q で検索、type / company_id / task_id で絞り込み、ページ分割、または id / ids で直接取得）、asset_neighbors。資産は全体で共有しているので、常に全件取得することは避けてください。

2. **目標の達成判定（中核の役割）**：goals フィールドに目標と状態があります。検出結果 / 事実によって達成が証明された未達成の目標には、prove_goal(goal_id, evidence_id, reason) を呼び出して met を設定してください。**最後の未達成目標を met にすると、システムはタスク全体を自動で完了と判定します**。終了処理は目標ごとの prove_goal だけで進め、別の「一括完了」手段はありません。
   - ⚠️ **定量的な達成条件の確認（早まった達成判定を厳禁とする）**：目標に定量的な条件（カバレッジ X%、N 個の flag、特定の権限の取得）がある場合、prove_goal の前に、上記 graph_overview の実測値（coverage.pct、findings_total の件数など）を【必ず】照合してください。未達なら prove_goal は【禁止】し、不足を補うインテントを追加してください。「おおむね達成 / 中核は確保できた」という理由で早く met にしてはいけません。例：必要なカバレッジが100%で、実測 coverage.pct=40% の場合 → 未達成として、追加テストのインテントを作成します。

3. **（任意。開始直後だけ、ごく軽量に）対象の把握**：グラフに fact がほぼなく（recent_facts がほぼ空で、タスク開始直後）、状況だけでは最初のインテントを具体化できない場合に限り、Bash などでごく少数の読み取り専用の確認を行って構いません（例：curl を1～2回実行してトップページ / フィンガープリントを確認）。**許される成果は、インテントをより正確に説明する1文だけです**。脆弱性の発見 / 検証 / 悪用や、エンドポイント / ディレクトリ / パラメーターの列挙結果を成果にしてはいけません（それらは worker の作業です。インテントとして追加してください）。次の3つの境界を厳守してください：
   - worker が生成した fact がすでにある場合（facts>0 / recent_facts が空でない）→ 自分で追加調査することは【禁止】です。すべて既存の fact に基づいて判断し、このラウンドの成果は「新しいインテントの追加」または「終了」に限ります。手掛かりを深掘りしたければ、自分で curl を実行せず、worker が調べるインテントを追加してください。
   - 開始直後でも確認は最大3回（≤3）で終え、最初のインテントの具体化だけに使ってください。「素早い方向決定」ではなく「詳細な検証」になっていると気付いたら、直ちに止めてインテントにしてください（エンドポイント / ディレクトリの個別列挙、id の個別試行、デコードの連鎖、同じ API の反復調査、注入 / 権限逸脱 / 脆弱性に関するあらゆるテストや検証は、すべて worker の実作業です）。
   - 既存の事実や状況から判断できる場合は、確認の実行自体が不要です。

4. **追加する新しい方向を決める**：**ここでの「抑制」は【既存のインテントを重複させない】という意味であり、「できるだけ少なく追加する」という意味ではありません**。目標が未達成なら、「終了してよいか」ではなく、「目標へ進むために、より深く踏み込み、まだ扱われていない手段は何か」を考えてください。インテントは【自由に考える探索方向】であり、固定の種類やメニューではありません。既知の事実、資産、目標から方向を判断し、open + running + recent_done と1件ずつ比較してください：
   - 既存の open / running が扱う方向 → 処理中なので追加しません。
   - recent_done にある方向 → **各インテントの state を先に確認し、停止の理由を区別して判断します**：
     · **done（正常に完了）**：対応済みなので、そのまま再追加しません。行き止まりかどうかは state ではなく、yields の fact の結論で判断します。【実質的に新しい仕組み】（新しい事実 / 資産 / パラメーター / 明確に異なる手段）がある場合だけ再追加し、summary に前回との違いを明記してください。言い換えや「もう一度試せば成功するかもしれない」は理由にならず、再試行は禁止です。
     · **exhausted（予算を使い切って途中で停止し、一部だけを書き戻した）/ blocked（モデルやネットワークの障害で、ほとんど調査できていない）**：どちらも正常に終わっておらず、情報が不足しています。最初に get_worker_trace / get_worker_output で実際の到達点と障害箇所を確認し、次から選んでください。突破目前で予算切れ → 「前回の進捗から継続」を追加。外部障害だけが原因で実行できなかった（blocked でよくある）→ 同じ方向をそのまま再追加。毎回同じ箇所で止まる → 手段 / 方向を変更。判断の根拠は常に trace の実際の進捗であり、state 自体ではありません。
   - どのインテントも扱っていない完全に新しい方向 → 追加します。
   - 既知の方向をすべて open / running が扱っている → 追加せず終了します（実行中 / 待機中の work が進むのを待ちます）。ただし recent_done しかなく、open / running がなく、目標が未達成なら、冒頭の必須条件に従って新規または継続のインテントを必ず追加してください。
   - **カバレッジより深さを優先**：coverage は最低条件 / 達成条件であり、探索目標そのものではありません。高い価値のある入口（RCE / 権限昇格 / データ流出へつながる可能性）が見つかったら、カバレッジを均すために各資産を浅く調べるより、その経路を【深く検証する】インテントを優先してください。
   - **経路の多様性を保ち、早すぎる絞り込みを避ける**：目標が未達成で、既存のインテントが同じ経路 / 入口に集中し、【本質的に異なる】未対応の方向（別の入口 / 別の資産種別 / 別の悪用経路）があるなら、同じ経路の同義のインテントを足すより、その異なる方向を優先してください（言葉ではなく実質的な違いで判断）。その方向も既存インテントが扱っていれば追加しません。理想は、仕組みの異なる経路が2～3本並存し（例：「アップロード経由」と「認証回避経由」）、いずれかで【目標への前進】を示す証拠が得られてから、そこへ資源を集中することです。**ただし、多様性よりもプロンプト冒頭の【操作上の制約】が常に優先します**。制約で除外された入口 / ポート / ホスト / 操作は、本質的に異なるものであっても、決してインテントにしないでください。

   **順番に実行すべき悪用経路は、段階ごとに追加し、並列に分解しないでください。** 強い依存関係のある直列の手順（①→②→③で、後の手順が前の手順の実際の成果に依存する）は、一度に並列で割り当ててはいけません（まだ存在しない前提を得られない後続は、重複や空回りを起こします）。TodoWrite で経路全体を1手順1項目の Todo として記録し、このラウンドでは「前提が満たされた」手順（通常は最初の手順）だけを追加してください。fact が得られ、次回の起動時に Todo 一覧がプロンプトに添付されたら、次の手順を追加し、満たされた項目を completed にしてください。「同じ作業」を2件に分けてはいけません（「発生箇所の確認」と「その箇所での発生」は同じ手順です）。複数のインテントを並列にするのは、【並行して実施でき、互いに依存しない】方向（例：無関係な複数のエンドポイントの列挙）だけです。

5. **登録**：選んだ新しい方向を add_intent の【1回の呼び出し】でまとめて登録してください（intents 配列。価値の高いものを最大4件。1件ずつ何度も呼び出さない）：
   - **summary**：その方向を自然な言葉の1文で説明します（テスト対象の完全なアドレス + 行うこと + 理由）。固定の分類に当てはめないでください。重複判定は主に、この文と既存インテントの比較で行います。
   - **asset_ids**：この方向でテスト / 攻撃する対象資産の id（list_assets から取得し、できるだけ指定。0件 / 1件 / 複数件）。具体的な資産（サイト / API / パラメーター / ホスト）を扱う場合は、対応範囲の重複排除と資産への関連付けに必要なので必ず指定してください。複数資産にまたがる場合はすべて指定します。具体的な資産がない全体的な調査だけの場合に限り、空欄にしてください。
   - **parent_ids**：この方向を導く根拠となった上流ノード（任意。0件 / 1件 / 複数件）。複数の事実を組み合わせて1つのインテントを導いたら、すべて指定します。上流のインテント / 検出結果から派生した場合も、その id を指定します。最上位の完全に新しい方向なら空欄にしてください。

重複や無理な追加を避けつつ、目標が未達成で、未対応のより深い手段があるときは追加してください。簡潔に、焦点を絞って、効率よく進めてください。`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 领域工具 + 基础默认工具集（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 资产覆盖度功能关闭时剔除 add_task_scope/list_untested_assets（不入 prompt）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 关键态势（刚完成的意图 + 预取的完整图）改放【本轮 user 输入】(见下方 input)，system
	// 只留静态规划正文。move-out 让 system 每轮稳定、更利于缓存；代价是若单轮变长，态势可能
	// 被 compaction 压缩（planner 单轮通常短，风险低）。situational 会拼进下方 input。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 任务级 deadline / 终局模式(经 ctx 注入,见 taskclock.go)。终局那一轮把任务超时
	// planner 收尾词作为【本轮操作指令】拼进本轮 user 输入(随 situational),让它只做最后
	// 目标判定、不产新意图。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【任务终局收尾（本轮特殊指令，覆盖上面的常规规划流程）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// 本任务的工作目录 <workDir>/tasks/<taskID>，先建好。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作约束(若有)注入系统提示,框定探索边界
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 无自身墙钟预算;有 deadline 时把 MaxDuration 夹逼到剩余,让在跑的规划轮在
	// 任务到点时进收尾(因超时→任务超时词,因步数→per-run 词)。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 走记录代理留痕；载入代理 CA 验证 MITM 重签的 HTTPS 证书
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 联网搜索(可选)。ddgs 无需 key；brave-free 需 BraveKey；tavily 需 TavilyKey。
		// WebSearchProxy 是独立出口代理(http/https/socks5)，与记录流量的 MITM 代理无关；空则直连。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 子命令默认走代理+信任 CA
		WorkingDir:            taskDir,                              // 本任务工作目录 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=不限;有 deadline 时=距 deadline 剩余
		Compaction:            compactionConfig(p.compactionWindow()),
		// 跨唤醒共享的规划待办：让串行链在多轮之间保留（session 是新的，store 不是）。
		Todos: p.todoFor(ts.ID()),
		// 命中【本轮】步数预算→ SDK 跑收尾:把本轮已想清楚的结论落地(该派的 add_intent、
		// 能证的 prove_goal、串行链记 TodoWrite),而非停止规划——planner 之后仍会被反复唤醒。
		// clamped(被任务 deadline 夹逼)时改用 PromptByReason(见 wrapupSettlementForTask)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = 不发上限,由服务端默认值决定
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 实验功能:开启后由 noa 接管上下文压缩(归档集中在 <workDir>/noa/<SessionID> 下,持久)。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 态势（刚完成的意图 + 完整图）现在拼进本轮 user 输入（见下方 input）。user 里还有
	// 指令 + 跨唤醒待办（todo 是模型自己的规划便签，可再生，放 user 即可）。
	// 开场白按「本轮有无具体变动」分两种：有变动 → 指向下方【实际变动】块；无变动
	// (心跳定时巡检 / hint / 恢复等) → 别谎称"图发生了变化",转而提示顺带复查在跑意图。
	lead := "刚有具体变动（见下面的【本次触发本轮的实际变动】），据此规划下一步："
	if len(triggers) == 0 {
		lead = "本轮是**定时巡检（心跳到点）/无具体变动信号**的唤醒——图不一定有新变动。顺带复查在跑意图：长时间无进展或跑偏的用 steer_work 纠偏、方向整个错的用 kill_work 止损；再判定目标、决定是否补方向："
		// 心跳/无变动唤醒时,若全图已无任何 open 或 running 意图 → 探索已停摆(没 worker 在跑、
		// 也没排队方向)。明确告知 planner 并强制其本轮补出新方向,别只复查在跑意图后空转一轮。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "本轮是**定时巡检（心跳到点）**的唤醒,且当前**已没有任何 open 或 running 的意图**——没有 worker 在跑、也没有排队中的方向,探索已停摆。你**必须**在本轮产出一个或多个向目标推进、且与图中既有意图**互不重复**的新意图(不得产出 0 意图);先据下面的态势判定目标是否已达成,未达成则立即补方向："
		}
	}
	input := lead + situational + "\n\n据上面的态势，判定目标。目标已【真正达成】（已拿到目标成果/已确认目标漏洞）时用 prove_goal 逐个标记。**硬底线：只要目标尚未达成、且当前没有任何 open 或 running 意图（frontier_open=0 且 running_intents 为空），本轮就必须产出至少一个向目标推进的意图——此时没有在跑的 work 可等、也没有在排队的方向，产出 0 意图=任务停摆。仅当已有 open/running 意图在推进、或目标已达成时，本轮才可以不产出新意图。**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration 现在会在墙钟到点打断在跑工具并就地进收尾(在活 ctx 上),单轮卡死不再
	// 绕过收尾,无需外部硬 ctx 兜底。ctx 只承载 pause / kill / shutdown。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
