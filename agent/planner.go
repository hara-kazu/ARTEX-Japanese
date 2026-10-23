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
	b.WriteString("\n\n【計画のタスク一覧（起動をまたいで保持する、前回あなたが記録した内容）】：\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("これに沿って進めてください。【前提の手順が完了している / 依存する fact が存在する】次の手順だけをインテントとして追加し、TodoWrite で一覧を更新してください（fact で満たされた手順を completed にする）。一覧ですでに pending/in_progress の手順を重複して割り当てないでください。")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 要約).
//	"goal"    — the human (via メイン agent の set_goals) added one OR MORE goals in a
//	            single call (Goals = 今回追加した目標文、1+ 件；set_goals は一括処理をサポート).
//	"goal_deleted" — the human deleted a goal from 概要の目標管理 (Detail = 削除した目標文).
//	"goal_edited"  — the human edited a goal from 概要の目標管理 (OldGoal→NewGoal の文).
//	"cancelled" — the human deleted intent IntentID (Detail = 削除理由). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 専用：削除前に取得した意図の要約（物理削除後はノードが存在せず、再検索できない）
	Goals    []string // Kind=="goal" 専用：今回の set_goals が追加した目標文（1件または複数件）
	OldGoal  string   // Kind=="goal_edited" 専用：変更前の目標文
	NewGoal  string   // Kind=="goal_edited" 専用：変更後の目標文
	Hints    []string // Kind=="hint" 専用：今回の add_hint が追加したヒント文（1件または複数件）
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
	b.WriteString("\n\n【今回のラウンドを起動した実際の変更（先にここを確認し、方向を追加するか判断すること）】：")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人間（メイン agent）が目標を一件追加しました：%s。新たに達成すべき目標です。対応するインテントがまだなければ、これに基づいて探索方向を追加してください。", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人間（メイン agent）が目標を%d件追加しました：%s。いずれも新たに達成すべき目標です。対応するインテントがまだない目標それぞれに、探索方向を追加してください。", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人間（メイン agent）が方針のヒントを一件追加しました：%s。探索グラフに追加済みです。対応するインテントがまだなければ、これに基づいて探索方向を調整 / 追加してください。", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人間（メイン agent）が方針のヒントを%d件追加しました：%s。すべて探索グラフに追加済みです。それぞれに基づいて探索方向を調整 / 追加してください。", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人間が目標を削除しました：%s。この目標は削除済みです。残りの目標 / 方向を再判定してください（この目標へのインテント追加は不要です）。", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人間が目標を「%s」から「%s」に変更しました。新しい目標に基づいて探索方向を調整してください（元の方向が適さなくなった場合は、その割り当てを止めてください）。", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- インテント #%d（%s）の worker が finding を一件報告しました：%s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意図内容は削除時に取得した Summary を優先（物理削除後はノードが存在せず、intentSummary で検索できない）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- インテント #%d をユーザーが削除しました。内容：%s、削除理由：%s。このインテントは削除済みで、実行されません。これに基づいて再計画してください。", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- インテント #%d（%s）の worker が終了しました。出力した結論：%s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("；このインテントで新しく生成した事実の id：%s ", fids))
			}
		}
	}
	b.WriteString("\n（詳細全体は node_detail / get_worker_output / list_findings で確認できます。）")
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
		return "（出力を取得できませんでした）"
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
		return "（この work にはまだ出力記録がありません）"
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
	return string(r[:n]) + " …（切り詰め済み。全文は get_worker_output で確認）"
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
	return "\n\n【今回のラウンドの状況（graph_overview の先行取得。このツールを呼び出した場合と同じ返り値です。詳細が必要な場合は node_detail/list_facts などを呼び出してください）】：\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (区分 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間成果物の出力規約
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `あなたはサイバーセキュリティ基盤上で、許可を得たペネトレーションテストの「計画」を担当します。グラフが変わるたびに起動されます。状況を把握し、目標の達成を判定し、**まだ扱われていない新しい方向が実際にある場合だけ**探索インテントを追加してください。あなたは計画担当であり、実行担当ではありません。このラウンドの成果は【インテントの作成 / 具体化】または【目標の達成判定】に限り、plan の中で実作業を済ませてはいけません。

タスクの目標：{{.Goal}}

**このラウンドで作成するインテントの数（最初に判断すること）**：
- **必須条件（最優先）**：【目標が未達成】かつ【open または running のインテントが1件もない】場合（frontier_open=0 かつ running_intents が空）、このラウンドで目標へ進むインテントを少なくとも1件【必ず】作成してください。実行中の work も待機中の方向もない状態で0件にすると、タスクが停止してしまいます。既知の方向が recent_done にしかなくても、以下の done / exhausted / blocked の判定に従って新しい方向を追加するか、継続のインテントを追加してください。
- 上の必須条件に該当しなければ、**インテントが0件でも正常ですが、正当な理由が必要です**。「少ない方が安定する」という理由で0件を既定にしないでください。正当な理由は、①**すでに対応中**：考えた方向はすべて open / running のインテントが扱っている（既存インテントの言い換えによる重複作成は重大な誤り）、②**依存する結果を待機中**：次の作業は実行中の work の成果に依存し、その成果がまだない（無理に追加すると後続が前提を得られず空回りするため、グラフ更新による次回の起動を待って追加する）、のいずれかです。
- 逆に、【未対応で、実行中の work に依存しない】新しい方向がある場合や、目標が未達成で範囲内に未テストの領域が残る場合は、インテントを追加してください。作業を省くために0件を既定にしてはいけません。

**起動されるたびの判断手順**：

1. **状況の全体像をこのプロンプトの下に添付しています**（graph_overview の返り値なので、再度呼び出す必要はありません）：task（元のタイトル + 目標 / ルートノード）、資産数、goals と状態、open / running / recent_done のインテント、sites_without_endpoints（エンドポイントがないサイトで、探索が必要かもしれない方向を示します）、facts（探索で得た事実の数。脆弱性とは別の種類）、recent_facts（{id,summary,confidence?}）。
   - **範囲**：探索ノード（goals / インテント / facts / findings）はこのタスクだけを含みます。**資産グラフは全体で共有**します（複数のタスクで共通のグラフを使用し、資産数は全体の範囲内の数であり、このタスク専有の数ではありません）。このタスクと関係のない資産は無視してください。
   - **派生関係**：各インテントには parents（派生元の事実 / インテント）と yields（そのインテントが生成した事実 / 検出結果）があり、recent_facts の各項目には from_intent があります。これを使い、どの方向からどの事実が得られたか、それらを組み合わせて新しい方向を導けるかを判断してください。
   - **否定的な観察 / 疑わしい観察**（recent_facts の「ポートが閉じている / 注入できない」など）は worker の観察であり、確定した結論ではありません。採用する前に node_detail(id) で evidence を確認してください。evidence が確かで、confidence=observed であり、手段を試し尽くしている場合だけ、その方向は当面進めないと判断します。evidence がない、「そう見える / 1回しか試していない」、または confidence=inferred の場合は【未解明】として扱い、範囲内でほかのインテントも扱っていなければ、原則として再確認のインテントを1件追加し、観察を裏付けるか覆してください（**同じ否定的な方向の再確認は最大1回**。再確認後も否定的で証拠が妥当なら、その結論を尊重し、それ以上追加しません）。
   - **詳細が必要な場合だけ呼び出すツール**：list_facts（ページ分割、新しい順、既定20件。q で絞り込み、before でページ移動。total / has_more 付き）、list_findings（すべての脆弱性）、node_detail(id)（完全な証拠 / 詳細。一覧 / recent_facts は要約のみ）、list_assets（pull：必要な分だけ取得。q で検索、type / company_id / task_id で絞り込み、ページ分割、または id / ids で直接取得）、asset_neighbors。資産は全体で共有しているので、常に全件取得することは避けてください。

2. **目標の達成判定（中核の役割）**：goals フィールドに目標と状態があります。検出結果 / 事実によって達成が証明された未達成の目標には、prove_goal(goal_id, evidence_id, reason) を呼び出して met を設定してください。**最後の未達成目標を達成済みにすると、システムはタスク全体を自動で完了と判定します**。終了処理は目標ごとの prove_goal だけで進め、別の「一括完了」手段はありません。
   - ⚠️ **定量的な達成条件の確認（早まった達成判定を厳禁とする）**：目標に定量的な条件（カバレッジ X%、N 個の flag、特定の権限の取得）がある場合、prove_goal の前に、上記 graph_overview の実測値（coverage.pct、findings_total の件数など）を【必ず】照合してください。未達なら prove_goal は【禁止】し、不足を補うインテントを追加してください。「おおむね達成 / 中核は確保できた」という理由で早く met にしてはいけません。例：必要なカバレッジが100%で、実測 coverage.pct=40% の場合 → 未達成として、追加テストのインテントを作成します。

3. **（任意。開始直後だけ、ごく軽量に）対象の把握**：グラフに fact がほぼなく（recent_facts がほぼ空で、タスク開始直後）、状況だけでは最初のインテントを具体化できない場合に限り、Bash などでごく少数の読み取り専用の確認を行って構いません（例：curl を1～2回実行してトップページ / フィンガープリントを確認）。**許される成果は、インテントをより正確に説明する1文だけです**。脆弱性の発見 / 検証 / 悪用や、エンドポイント / ディレクトリ / パラメーターの列挙結果を成果にしてはいけません（それらは worker の作業です。インテントとして追加してください）。次の3つの境界を厳守してください：
   - worker が生成した fact がすでにある場合（facts>0 / recent_facts が空でない）→ 自分で追加調査することは【禁止】です。すべて既存の fact に基づいて判断し、このラウンドの成果は「新しいインテントの追加」または「終了」に限ります。手掛かりを深掘りしたければ、自分で curl を実行せず、worker が調べるインテントを追加してください。
   - 開始直後でも確認は最大3回（≤3）で終え、最初のインテントの具体化だけに使ってください。「素早い方向決定」ではなく「詳細な検証」になっていると気付いたら、直ちに止めてインテントにしてください（エンドポイント / ディレクトリの個別列挙、id の個別試行、デコードの連鎖、同じインターフェースの反復調査、注入 / 権限逸脱 / 脆弱性に関するあらゆるテストや検証は、すべて worker の実作業です）。
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

   **順番に実行すべき悪用経路は、段階ごとに追加し、並列に分解しないでください。** 強い依存関係のある直列の手順（①→②→③で、後の手順が前の手順の実際の成果に依存する）は、一度に並列で割り当ててはいけません（まだ存在しない前提を得られない後続は、重複や空回りを起こします）。TodoWrite で経路全体を1手順1項目のタスク一覧として記録し、このラウンドでは「前提が満たされた」手順（通常は最初の手順）だけを追加してください。fact が得られ、次回の起動時に タスク一覧がプロンプトに添付されたら、次の手順を追加し、満たされた項目を completed にしてください。「同じ作業」を2件に分けてはいけません（「発生箇所の確認」と「その箇所での発生」は同じ手順です）。複数のインテントを並列にするのは、【並行して実施でき、互いに依存しない】方向（例：無関係な複数のエンドポイントの列挙）だけです。

5. **登録**：選んだ新しい方向を add_intent の【1回の呼び出し】でまとめて登録してください（intents 配列。価値の高いものを最大4件。1件ずつ何度も呼び出さない）：
   - **summary**：その方向を自然な言葉の1文で説明します（テスト対象の完全なアドレス + 行うこと + 理由）。固定の分類に当てはめないでください。重複判定は主に、この文と既存インテントの比較で行います。
   - **asset_ids**：この方向でテスト / 攻撃する対象資産の id（list_assets から取得し、できるだけ指定。0件 / 1件 / 複数件）。具体的な資産（サイト / インターフェース / パラメーター / ホスト）を扱う場合は、対応範囲の重複排除と資産への関連付けに必要なので必ず指定してください。複数資産にまたがる場合はすべて指定します。具体的な資産がない全体的な調査だけの場合に限り、空欄にしてください。
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
	// ドメインツール + 基本のデフォルトツール群（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産被覆率機能が無効なら add_task_scope/list_untested_assets を除外（prompt に入れない）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 重要な状況（直近完了の意図 + 先読みした全グラフ）は【今回の user 入力】に移し(下の input を参照)、system は
	// 静的な計画本文のみ保持する。move-out によって system が毎回安定し、キャッシュしやすくなる；ただし1ラウンドが長くなると状況が
	// compaction で圧縮される可能性がある（planner の1ラウンドは通常短く、低リスク）。situational は下の input に連結する。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// タスク単位の deadline / 最終モード(ctx 経由で注入、taskclock.go を参照)。最終ラウンドではタスクのタイムアウト用
	// planner 終了プロンプトを【今回の操作指示】として今回の user 入力に連結(situational と一緒に)し、最後の
	// 目標判定のみ行わせ、新しい意図は生成しない。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【タスクの最終終了処理（このラウンドの特別な指示で、上記の通常の計画手順に優先します）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// このタスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作成する。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作制約(あれば)をシステムプロンプトに注入し、探索境界を定める
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 自体の実時間予算はない;deadline がある場合は MaxDuration を残り時間に制限し、実行中の計画ラウンドが
	// タスク期限到達時に終了処理に入るようにする(タイムアウト→タスクのタイムアウト文、ステップ数→per-run 文)。
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
		EnableWebFetch:  true, // 記録プロキシ経由で履歴を残し、プロキシ CA を読み込んで MITM が再署名した HTTPS 証明書を検証する
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// ネットワーク検索(任意)。ddgs は key 不要；brave-free は BraveKey が必要；tavily は TavilyKey が必要。
		// WebSearchProxy は独立した送信プロキシ(http/https/socks5)で、トラフィック記録用の MITM プロキシとは無関係；空なら直接接続。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash サブコマンドはデフォルトでプロキシ経由+CA を信頼
		WorkingDir:            taskDir,                              // このタスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=無制限;deadline がある場合=deadline までの残り時間
		Compaction:            compactionConfig(p.compactionWindow()),
		// 起動間で共有する計画 TODO：直列チェーンを複数ラウンド間で保持する（session は新規だが、store は既存）。
		Todos: p.todoFor(ts.ID()),
		// 【今回】のステップ数予算に到達→ SDK が終了処理:今回固まった結論を実行(割り当てる add_intent、
		// 証明可能な prove_goal、直列チェーンを TodoWrite に記録)し、計画を停止しない——planner はその後も繰り返し起動される。
		// clamped(タスクの deadline で制限)の場合は PromptByReason を使用(wrapupSettlementForTask を参照)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // この profile が非ストリーミングを選択した場合は Provider.Complete を使用
		MaxTokens:    p.maxTokens(),    // 0 = 上限を送信せず、サーバー側のデフォルト値に委ねる
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 実験機能:有効時は noa がコンテキスト圧縮を担当(アーカイブは <workDir>/noa/<SessionID> 配下に集約し、永続化)。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 状況（直近完了の意図 + 全グラフ）は今回の user 入力に連結する（下の input を参照）。user にはさらに
	// 指示 + 起動間の TODO（todo はモデル自身の計画メモで再生成可能なので、user に置けばよい）がある。
	// 導入文は「今回具体的な変動があるか」で2種類：変動あり → 下の【実際の変動】ブロックを参照；変動なし
	// (ハートビートの定期点検 / hint / 再開など) → "グラフが変わった"と偽らず、実行中の意図も確認するよう促す。
	lead := "具体的な変更がありました（下記の【今回のラウンドを起動した実際の変更】を参照）。これに基づいて次の手順を計画してください："
	if len(triggers) == 0 {
		lead = "今回は**定期確認（ハートビートの時刻）/ 具体的な変更通知がない状態**での起動です。グラフに新しい変更があるとは限りません。実行中のインテントも再確認し、長時間進展がないものや方向がずれたものは steer_work で修正し、方向そのものが誤っているものは kill_work で打ち切ってください。その後、目標を判定し、方向を追加するか決めてください："
		// ハートビート/変動なしの起動時に、全グラフに open または running の意図がなければ → 探索は停止している(worker の実行も、
		// 待機中の方向もない)。planner に明示し、今回新しい方向を補わせ、実行中意図を確認するだけの空回りを防ぐ。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "今回は**定期確認（ハートビートの時刻）**による起動で、現在は **open または running のインテントが一件もありません**。実行中の worker も待機中の方向もなく、探索が停止しています。このラウンドで、目標へ進み、グラフの既存インテントと**重複しない**新しいインテントを一件以上作成することが**必須**です（0件にしてはいけません）。まず下記の状況から目標が達成済みか判定し、未達成なら直ちに方向を追加してください："
		}
	}
	input := lead + situational + "\n\n上記の状況に基づいて目標を判定してください。目標が【実際に達成された】場合（目標の成果を得た / 目標の脆弱性を確認した場合）は、prove_goal で個別に記録してください。**必須条件：目標が未達成で、現在 open または running のインテントが一件もない場合（frontier_open=0 かつ running_intents が空）、このラウンドでは目標へ進むインテントを少なくとも一件必ず作成してください。この状態では待つべき実行中の work も待機中の方向もなく、インテント0件はタスクの停止を意味します。open/running のインテントがすでに進行中か、目標が達成済みの場合に限り、このラウンドで新しいインテントを作成しなくても構いません。**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration は実時間の期限で実行中のツールを中断し、その場で終了処理に入る(有効な ctx 上)。1ラウンドの停止で
	// 終了処理を迂回しなくなり、外部の強制 ctx による保護は不要。ctx は pause / kill / shutdown のみ担う。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
