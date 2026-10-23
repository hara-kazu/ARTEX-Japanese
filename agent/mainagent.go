package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (区分 [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間
// 成果物の出力規約 tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `あなたは許可を得たペネトレーションテストシステムの「メイン agent」であり、人間の操作担当者との窓口です。自ら探索したり、自律的にインテントを作り続けたりしないでください（それは計画エージェントの役割です）。担当する作業は次のとおりです：

1. 状況の確認：graph_overview / list_findings / list_facts / list_assets / get_worker_output を使用し、現在の進捗に関する質問へ回答します。
2. 方針の調整（人間の指示をシステムへ反映する）：
   - 「方向を変えたい / 特定の脆弱性を重視したい / 特定の領域に集中したい」という指示 → add_hint でヒントを書き込みます（計画エージェントが次回に読み取ります）。
   - 「特定の対象を今すぐテストしたい」という指示 → add_intent で優先度の高いインテントを1件直接追加します（priority 8-10）。システムは完了済みのタスクを自動で実行中へ戻し、worker がそのインテントを実行します。実行後は完了状態へ戻ります。
     **タスクの目標がすべて達成済みの場合**（graph_overview の goals がすべて met）：追加する前に、そのインテントに「新しく達成したい結果」が含意されていないか判断してください。含意されている場合は、推測した目標を1文でユーザーに示し、**正式な目標として登録するか確認してください**。登録する場合 → set_goals で登録します（通常の計画へ移行し、計画エージェントが自律的に進めます）。登録しない場合 / 一時的に調べたいだけの場合 → add_intent でその1件だけを追加します。worker の実行後はタスクが完了状態へ戻り、自律的には続行しません。明らかに1回限りの確認で、新しい目標を含意していなければ、直接 add_intent を使用して構いません。毎回確認する必要はありません。
   - 「実行中のインテント（work）をその場で修正したい（X をやめて Y に集中してほしい）」という指示 → steer_work を使用します（実行を中断せず、既存の進捗を失わず、worker の次の操作前に反映されます）。最初に get_worker_output で現在の作業を確認してください。方向そのものが誤っている場合は、add_intent で別のインテントを追加します。
   - 「最終的に達成したい目標を追加したい」という指示 → set_goals で目標を追加します。システムは目標をタスクのグラフへ保存し、**完了済み / 一時停止中のタスクを自動で実行中へ戻します**（計画エージェントが改めて達成状況を判定します）。ユーザーが再開ボタンを押す必要はありません。
   - 「テストの制約を追加 / 変更したい（特定の操作を許可 / 禁止する。例：『現在のポートだけをテスト』『総当たりを禁止』『受動的な調査だけを実施』）」という指示 → set_constraints で登録します（type=allow は許可、type=deny は禁止）。制約は次回の計画時に planner / worker のプロンプトへ挿入され、探索の境界を定めます。概要の「操作の制約」から追加 / 削除 / 変更することもできます。
3. 分かりやすい言葉で簡潔に返答し、実施した作業を説明します。

現在のタスクの目標：{{.Goal}}

検出結果を創作しないでください。ツールが返した実際のデータだけに基づいて回答してください。`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 汎用の起動通知（専用コールバックがない書き込み操作はこれを使用、debounced）
	tsx.SetResumeTask(resume)     // set_goals で目標追加 → 完了/一時停止したタスクを running に戻す
	tsx.SetNotifyGoal(notifyGoal) // set_goals で目標追加 → planner に「人が目標を追加：…」のトリガーを記録
	tsx.SetNotifyHint(notifyHint) // add_hint でヒント追加 → planner に「人が N 件の戦略ヒントを追加：…」のトリガーを記録
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// ドメインツール + 基本のデフォルトツール群（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産被覆率機能が無効なら add_task_scope/list_untested_assets を除外（prompt に入れない）。
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// このタスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作成する。
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシ経由で履歴を残し、プロキシ CA を読み込んで MITM が再署名した HTTPS 証明書を検証する
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// ネットワーク検索(任意)。ddgs は key 不要；brave-free は BraveKey が必要；tavily は TavilyKey が必要。
		// WebSearchProxy は独立した送信プロキシ(http/https/socks5)で、トラフィック記録用の MITM プロキシとは無関係；空なら直接接続。
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash サブコマンドはデフォルトでプロキシ経由+CA を信頼
		WorkingDir:            mainDir,                              // このタスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // セッション単位の一時的な TODO（TodoWrite）で、計画専用、終了時に破棄
		// 予算(ステップ数)に到達→ SDK が終了処理:ユーザーへ1文の進捗要約を出力。Prompt と終了ラウンド数は管理画面で編集可能(デフォルト 10 ラウンド)。
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // この profile が非ストリーミングを選択した場合は Provider.Complete を使用
		MaxTokens:    m.maxTokens(),    // 0 = 上限を送信せず、サーバー側のデフォルト値に委ねる
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 実験機能:有効時は noa がコンテキスト圧縮を担当(アーカイブは <workDir>/noa/<SessionID> 配下に集約し、永続化)。
	// session id は transcript と同じルール(セグメントを認識)とし、アーカイブと復元を揃える。
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
