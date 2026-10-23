package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// deepseek バックエンドは他の3つとは性質が異なる：DeepSeek には直接呼び出せる検索 API がなく、
// 検索は Anthropic 互換の messages API 内部(web_search_20250305 server
// tool)だけにあるため、検索ごとにモデル呼び出しを1回消費し、検索リクエストは DeepSeek サーバーから送られる——
// ローカルの Proxy を通らず、トラフィック履歴にも入らない。
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* は現在有効な LLM 設定に由来(anthropic 形式の DeepSeek 公式端点のみ)し、
	// 単独では設定せず、LLM 設定の切り替えに伴って変わる。
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "実行枠を使い切るため、まもなく終了します。追加のコマンドや調査を実行しないでください。次の順序で処理してください：(1) ここまでに特定した未保存の内容を1件ずつ書き戻します。新しい資産は insert_assets、探索の結論や事実は record_fact、確認済みの脆弱性は report_finding を使ってください。(2) **最後に、独立したプレーンテキストの1文で**、実施した作業と主な結論を要約してください。この1文を今回の実行結果として表示するため、必ず出力してください。"

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (区分 [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 中間成果物の出力規約
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (区分 [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `あなたはサイバーセキュリティ基盤上で、許可を得たペネトレーションテストを「実行」する work agent です。【1件のインテント】（探索方向を示す1文）が割り当てられます。唯一の役割は、**そのインテントを完了し、検出結果を知識グラフに書き戻し、実行を終了して返すことです。**

**厳守する境界**：
1. **割り当てられた1件のインテントだけを実行してください**。**その調査中に、担当範囲外で深掘りする価値のある手掛かりが見つかったら**（エラーで漏れたパス、別の資産と連携する可能性のある箇所、別の悪用経路と思われる入口）、**fact の summary に1文で記載して計画エージェントへ引き継いでください**。
2. 最初に阻まれたこと（payload がフィルターで阻まれる / 404 / 注入結果が返らないこと）は、調査を尽くしたことを意味しません。このインテントで使える回避手段をすべて試してから結論を出してください。
3. 許可された範囲内だけで操作してください。システムプロンプトの冒頭に【操作上の制約】がある場合は、それが最優先の境界です。各コマンド / 調査の実行前に自分で照合し、違反する操作は行わないでください（割り当てられたインテントに含まれていても同様です）。

**発見するたびに書き戻してください**（グラフへの保存だけが有効で、頭の中や文章に書いただけでは保存になりません。結果を得たら即座に保存し、最後まで溜めてステップ数切れで失わないでください）。書き戻しは次の3種類です。保存先のグラフを混同しないでください：
- **新しい資産 / リソース → insert_assets（資産グラフ）**：サブドメイン / service / endpoint / フィンガープリント / 認証情報など、すべての資産【そのもの】を登録します。**ここには資産だけを登録し、探索の結論や判断は record_fact に記録してください。**
- **探索の結論 / 事実 → record_fact（探索グラフ。intent_id を渡す）**：すべてこのツールで記録します。**複数の観察を【1件】の事実へまとめてください**（summary に1文の要約、detail に実際の実行過程に基づく詳しい説明）。属性ごとに分割せず、通常は1インテントにつき1件にします。細分化するとグラフが際限なく膨らむため、**原則は1件とし、detail にまとめられるものはすべてまとめてください**。【互いに完全に独立し、まとめられない】結論が実際にある場合だけ、facts 配列で分けて記録します。これはごく少数の例外であり、通常の方法ではありません。**差分だけを記録**：今回【新しく得た】内容だけを記録し、既存の事実を言い換えて再登録しないでください（既存内容の裏付けだけで新規情報がなければ、記録は不要です）。**実際に観察した内容だけを記録**：evidence を1行で添え（コマンド + 最も証明力のある出力1～2行。簡潔にし、詳細は detail）、confidence を指定してください（observed=直接観察 / inferred=現象からの推測）。
- **確認済みの脆弱性 → report_finding（探索グラフ。PoC を含め、intent_id を渡す）**：**今回の実行で実際に発生させ、再現可能な証拠（リクエスト / レスポンスまたはコマンド出力）を得た場合だけ使用してください**。「バージョン / フィンガープリントが CVE と一致した」「パラメーターが注入できそうに見える」「外部の脆弱性データベース / 変更履歴 / コードの diff から推測した」を確認済みとすることを厳禁とします。CVE の検索やパッチ適用バージョンの比較を、実際の発生確認の代わりにしてはいけません。発生させられないが疑いがある場合 → record_fact で inferred の事実を1件記録し（疑わしい箇所 + 発生させられなかった理由）、計画エージェントへ渡してください。無理に finding として記録しないでください。


インテントを完了したら、実施したことと書き戻した事実を1文でまとめてください。`

// workerTrafficBlock is 区分 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**トラフィックのツール**：\n- traffic_search / traffic_get / traffic_blob：応答の再確認や、アクセス済みリソースの検索に使用します。**先にトラフィックを調べ、同じ URL に curl を繰り返さないでください**。traffic_search は **host の指定が必須**で、既定では非常に軽量な索引を3件だけ返します（id/method/url/status/resp_len。応答内容は含みません）。さらに必要なら limit を明示的に増やしてください。body_contains でリクエスト / レスポンス本文を全文検索できます（3文字以上。部分文字列と中国語に対応し、パスワード / 鍵 / エラー / 内部ネットワークのアドレスなどを検索）。個々の原文は traffic_get(id) で取得します。非常に大きい本文は @blob sha256:<hash> と表示されるため、traffic_blob(hash) で全文を分割して取得してください。"
}

// artifactSpec is 区分 [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**中間生成物の出力規約**：スクリプト、payload、取得した応答本文、一時データなど、すべての中間生成物は、**必ずこのタスクの作業ディレクトリに書き込んでください：" + dir + "**（相対パスならここに書き込まれます。この絶対パスも使用できます）。**/tmp に書き込んだり、他の絶対パスを使用したりしないでください**。"
}

// workerArtifactSpec is the worker's 区分 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**中間生成物の出力規約**：スクリプト、payload、取得した応答本文、一時データなど、すべての中間生成物は、**必ず今回のインテント専用の作業ディレクトリに書き込んでください：" + runDir + "**（作成済みなので、相対パスでここに直接書き込めます。手動でのディレクトリ作成は不要です）。**/tmp に書き込んだり、他の絶対パスを使用したりしないでください**。"
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【割り当てられたインテント（今回の唯一の作業：この一件だけを実行し、事実だけを生成し、完了したら停止する）】：\n%s\nインテント id: %d（record_fact / report_finding に書き戻す際に渡してください）", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage は計画担当が「テストが少ない型 / 範囲拡大の要否」を判断する信号で、worker の「取得した
	// 意図だけを実行し、未被覆の箇所を追わない」という責務境界に反するため、worker の表示から除外する。data は今回の worker
	// 専用の新しい map で、キー削除は planner に影響しない。
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【探索全体の状況（読み取り専用。自分のインテントを全体の中で捉えるための情報）】：\n" +
		"以下はタスク全体の現在の探索概要です。他の担当者の発見を把握して重複を避けることと、自分のインテントを探索する際に全体との関係を考えることの二つに使います。\n" +
		"**発想を広げるのは良いことです**。このインテントの探索中は、深く考え、多くの関連を考えてください。唯一の境界は、他のインテントを実際に実行しないことです（他の worker の作業であり、計画エージェントが割り当てます）。価値のある手掛かり（資産をまたぐ連携、別の悪用経路と思われる入口、全体に関わる疑わしい箇所）を思い付いたら、**必ず fact に記録して計画エージェントへ渡してください**。これは任意の付加情報ではなく、重要な成果です。自分だけで留めるより、一件多く報告して計画エージェントの判断に委ねてください。\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding の保存時にその場で planner を起動し、「どの意図+finding」かを渡す
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker には意図的に MultiEdit/Glob/Grep を与えない：ファイルの細かな編集は Edit、検索は Bash(grep/find) に集約し、
	// ツールの範囲を絞って低価値の呼び出しを減らす。他の SDK デフォルトツール(Read/Write/Edit/LS/Bash/Sleep)は通常どおり。
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 意図は worker の【唯一の責務で、run 全体を貫く不変条件】なので、起動指示と、意図が固定する対象資産の
	// 元データとともに system prompt に入れる：system は run ごとに再構築し、compaction では決して圧縮されないため、
	// 長い run でも意図は常に存在し、再開時も transcript 履歴に最初のメッセージが残っているかに依存しない。代償として system に
	// per-intent の可変データが混ざり、意図間のキャッシュ再利用を失うが、意図した選択（意図の喪失は token 節約よりはるかに重大）。
	// planner の「状況ブロックは user turn に置く」と分けるのは意図的：planner 自身は意図を生成する側で単一の mandate がないが、
	// worker にはある。【全体状況 overview】のみ起動 user メッセージに残す——格下げ可能で stale を許容でき、圧縮されても問題ない。
	// 今回の意図専用の作業ディレクトリ <workDir>/tasks/<taskID>/i<intentID> は、エンジン側で事前に作成する。
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作制約(あれば)をシステムプロンプトに注入し、worker は実行時に厳守する
	}
	// 意図ブロック → 意図の固定資産ブロック → 起動指示の順で system 末尾に追加（constraintBlock と同じ追加方法）。
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\nこのインテントの asset_ids に対応する対象資産：\n" + string(b)
				}
				// 意図が明示的に対象とするこれらの資産 → 自動でタスクのテスト範囲に追加（insertAssets と同じ
				// 保守的な粒度）。upsertTaskScope の ON CONFLICT DO NOTHING + uq_task_scope の
				// 一意インデックスで重複追加を防ぎ、再実行/再試行も冪等な no-op となる。
				// 資産被覆率機能が無効な場合、テスト範囲(分母)を蓄積しない。
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n上記のインテントの実行を開始してください。この一件だけを実行し、事実、assets、finding だけを生成し、完了したら停止してください。"
	system, boundary := deferredSystem(sysBody, def)
	// タスク単位の deadline(ctx 経由で注入)がこの run の実時間予算を制限し、終了プロンプトを決める(taskclock.go を参照)。
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch は記録プロキシを通し、HTTP は curl と同様に履歴を残す；プロキシ CA を読み込んで MITM が
		// 再署名した HTTPS 証明書を【通常どおり検証できる】ようにする（検証を無効にしない）。proxy が空なら直接接続。
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// ネットワーク検索(任意)。ddgs は key 不要；brave-free は BraveKey が必要；tavily は TavilyKey が必要。
		// WebSearchProxy は独立した送信プロキシ(http/https/socks5)で、トラフィック記録用の MITM プロキシとは無関係；空なら直接接続。
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash サブコマンドの HTTP はデフォルトで記録プロキシ経由 + その CA を信頼（ツールの -x/-k は不要）。
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 実時間予算をラウンド境界で判定し、途中で中断しない;0 = 無制限。タスク単位の deadline がある場合は min(自身の予算、
		// deadline までの残り時間)に制限し、タスク期限時にこの run が自然に終了処理へ入るようにする(taskclock.go を参照)。
		MaxDuration: maxDur,
		// 予算(ラウンド数 OR 時間)に到達→ SDK が1ラウンドの終了処理を行い(Bash を非表示)、認識済み内容を書き戻し、未完了を防ぐ。
		// clamped(タスクの deadline で制限)の場合は PromptByReason を使用:タイムアウト=タスク期限到達→タスクのタイムアウト文、
		// ステップ数=制限ウィンドウ内で先にステップが尽きる→per-run 文に戻す。clamped でなければ純粋な per-run を維持。
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 切り詰め上限は SDK のデフォルト(30000 文字)を使用。
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // セッション単位の一時的な TODO（TodoWrite）で、計画専用、終了時に破棄
		NonStreaming:  w.nonStreaming(),                       // この profile が非ストリーミングを選択した場合は Provider.Complete を使用
		MaxTokens:     w.maxTokens(),                          // 0 = 上限を送信せず、サーバー側のデフォルト値に委ねる
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 意図 / 起動指示 / 意図の固定資産は system prompt とともに送信済み（上の sysBody の組み立てを参照）。
	// この起動 user メッセージは【全体状況 overview】のみ保持——格下げ可能な全体把握用情報なので、圧縮されても問題ない。
	// overview の marshal がまれに失敗して空の場合は、1文の起動文にフォールバックし、初回に空の user メッセージが出ることを防ぐ。
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "system で割り当てられたインテントの実行を開始してください。この一件だけを実行し、事実、assets、finding だけを生成し、完了したら停止してください。"
	}

	// 実験機能:有効時は noa がコンテキスト圧縮を担当(アーカイブは <workDir>/noa/<SessionID> 配下に集約し、永続化)。
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "実行を続けてください。"
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "前回の人間とのチャットで入力された新しいインテントの実行を続けてください。完了済みの操作を繰り返さないでください。"
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【人間とのチャットで入力された新しいインテント】\n" + message +
				"\n\nこの人間からの入力に従って直ちに実行し、完了後に文脈に基づいて元のタスクを続ける必要があるか判断してください。"
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【人間とのチャットで入力された新しいインテント】\n" + message +
				"\n\nこの人間からの入力を優先して実行してください。"
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
