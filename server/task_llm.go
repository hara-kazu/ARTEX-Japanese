package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type taskAgentBundle struct {
	runtime        *taskLLMRuntime // goal decomposition runtime
	plannerRuntime *taskLLMRuntime
	workerRuntime  *taskLLMRuntime
	mainRuntime    *taskLLMRuntime
	pl             *agent.Planner
	wk             *agent.Worker
	main           *agent.MainAgent
}

type llmAuditProfile struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Format string `json:"format"`
	Model  string `json:"model"`
}

type llmTransitionAudit struct {
	Mode     string           `json:"mode"` // automatic | manual | exhausted
	Reason   string           `json:"reason"`
	Previous *llmAuditProfile `json:"previous,omitempty"`
	Next     *llmAuditProfile `json:"next,omitempty"`
}

type llmActivityMetadata struct {
	LLMTransition llmTransitionAudit `json:"llm_transition"`
}

type taskLLMRuntime struct {
	s        *Server
	taskID   string
	agentKey string
}

type taskLLMError struct {
	taskID         string
	chainExhausted bool
	cause          error
}

func (e *taskLLMError) Error() string {
	if e.chainExhausted {
		return fmt.Sprintf("task %s LLM profile chain exhausted: %v", e.taskID, e.cause)
	}
	return e.cause.Error()
}

func (e *taskLLMError) Unwrap() error { return e.cause }

func isTaskLLMRuntimeError(err error) bool {
	var target *taskLLMError
	return errors.As(err, &target)
}

func isTaskLLMChainExhausted(err error) bool {
	var target *taskLLMError
	return errors.As(err, &target) && target.chainExhausted
}

// isQuotaExhaustedError is intentionally strict. A generic 429, auth error,
// network failure, or 5xx does not rotate providers; the response must explicitly
// identify quota, credits, billing balance, or payment exhaustion.
func isQuotaExhaustedError(err error) bool {
	return err != nil && agent.IsQuotaExhaustedMessage(err.Error())
}

type taskLLMSelection struct {
	task      *Task
	profileID int64
	revision  int64
	provider  llm.Provider
	// retry は今回選択した設定の解決済み再試行パラメーター(profile の上書き → グローバルポリシー → 組み込みデフォルト)。
	// 同じ provider の安全ウィンドウ内の再試行はこれに従うため、profile 変更で再試行の間隔も変わる。
	retry agent.RetryConfig
}

type taskLLMStreamHooks struct {
	current    func() (taskLLMSelection, error)
	exhaust    func(taskLLMSelection, error) (db.TaskLLMTransition, error)
	transition func(taskLLMSelection, db.TaskLLMTransition, error)
}

// current resolves the provider this role runs on, by precedence:
// Agent の紐付け → タスクの LLM 設定チェーン → グローバル/環境設定。
// 紐付けはタスクチェーンより優先：特定の役割へ明示的にモデルを指定したら、そのモデルで継続して実行し、紐付けがない、または
// 構築失敗の場合のみタスクチェーンへ格下げし、タスクチェーンが空ならさらにグローバル設定へ格下げする。
// 返す profile id はタスクチェーンを使う場合のみ非ゼロ —— streamTaskLLM はこれにより割当枠エラーが
// タスクのフェイルオーバー状態を進めるべきか判定する（紐付け/グローバル経路はタスクチェーン状態を変更せず、従来の意味を維持）。
func (r *taskLLMRuntime) current() (taskLLMSelection, error) {
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return taskLLMSelection{}, err
	}
	pt, err := r.s.m.pg.GetTask(taskNum)
	if err != nil {
		return taskLLMSelection{}, err
	}
	if pt == nil {
		return taskLLMSelection{}, fmt.Errorf("task %s not found", r.taskID)
	}
	r.s.syncTaskLLMState(pt)
	t, _ := r.s.m.Task(r.taskID)
	sel := taskLLMSelection{task: t, revision: pt.LLMChainRevision}
	if prov, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		sel.provider, sel.retry = prov, cfg.Retry
		return sel, nil
	}
	if len(pt.LLMProfileIDs) > 0 {
		if pt.ActiveLLMProfileID == nil {
			return sel, &taskLLMError{taskID: r.taskID, chainExhausted: true, cause: errors.New("all selected profiles are quota exhausted")}
		}
		sel.profileID = *pt.ActiveLLMProfileID
		prov, cfg, ok := r.s.providerForProfile(sel.profileID)
		if !ok {
			return sel, fmt.Errorf("LLM profile #%d is missing or invalid", sel.profileID)
		}
		sel.provider, sel.retry = prov, cfg.Retry
		return sel, nil
	}
	prov, cfg, ok := r.s.globalProvider()
	if !ok {
		return sel, fmt.Errorf("task %s has no available fallback LLM provider", r.taskID)
	}
	sel.provider, sel.retry = prov, cfg.Retry
	return sel, nil
}

// activeCfg resolves the task's currently-active LLM config, mirroring current()'s
// source precedence (agent binding → active chain profile → global). Read-only and
// best-effort: ok=false when nothing resolves, leaving the per-setting fallback to
// the caller. If failover switches profiles, the change takes effect on the next
// agent run (a fresh Session is built per run in captureRun).
func (r *taskLLMRuntime) activeCfg() (agent.Config, bool) {
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return agent.Config{}, false
	}
	pt, err := r.s.m.pg.GetTask(taskNum)
	if err != nil || pt == nil {
		return agent.Config{}, false
	}
	if _, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		return cfg, true
	}
	if len(pt.LLMProfileIDs) > 0 && pt.ActiveLLMProfileID != nil {
		if _, cfg, ok := r.s.providerForProfile(*pt.ActiveLLMProfileID); ok {
			return cfg, true
		}
	}
	if _, cfg, ok := r.s.globalProvider(); ok {
		return cfg, true
	}
	return agent.Config{}, false
}

// nonStreaming reports whether the task's currently-active LLM source is set to
// non-streaming. Unresolvable → streaming (false), the safe default.
func (r *taskLLMRuntime) nonStreaming() bool {
	cfg, ok := r.activeCfg()
	return ok && !cfg.Stream
}

// maxTokens returns the currently-active source's per-reply output cap.
// Unresolvable → 0, i.e. send no cap, matching the pre-setting behaviour.
func (r *taskLLMRuntime) maxTokens() int {
	cfg, _ := r.activeCfg() // 設定が解決されなければゼロ値 0
	return cfg.MaxTokens
}

func parseTaskID(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid task id %q", id)
	}
	return n, nil
}

// streamHooks builds the failover/exhaustion callbacks shared by Stream and
// Complete: how to read the current profile selection, how to mark it quota
// exhausted, and how to emit a failover transition.
func (r *taskLLMRuntime) streamHooks() taskLLMStreamHooks {
	return taskLLMStreamHooks{
		current: r.current,
		exhaust: func(selection taskLLMSelection, cause error) (db.TaskLLMTransition, error) {
			taskNum, _ := parseTaskID(r.taskID)
			transition, err := r.s.m.pg.MarkTaskLLMProfileQuotaExhaustedAtRevision(taskNum, selection.profileID, selection.revision, cause.Error())
			if err != nil {
				return transition, err
			}
			if pt, getErr := r.s.m.pg.GetTask(taskNum); getErr == nil && pt != nil {
				r.s.syncTaskLLMState(pt)
			}
			return transition, nil
		},
		transition: func(selection taskLLMSelection, transition db.TaskLLMTransition, cause error) {
			r.s.emitTaskLLMTransition(selection.task, transition, cause)
		},
	}
}

func (r *taskLLMRuntime) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	ctx = llmrec.WithTaskID(ctx, r.taskID)
	return streamTaskLLM(ctx, r.taskID, req, r.streamHooks())
}

// Complete is the non-streaming counterpart of Stream. A non-streaming call is
// atomic — it never delivers partial output — so every failure is safe to retry
// on the same provider or fail over to the next profile without risking
// duplicated model output or tool execution (the "committed" bookkeeping the
// streaming path needs is unnecessary here).
func (r *taskLLMRuntime) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx = llmrec.WithTaskID(ctx, r.taskID)
	return completeTaskLLM(ctx, r.taskID, req, r.streamHooks())
}

func completeTaskLLM(ctx context.Context, taskID string, req llm.CompletionRequest, hooks taskLLMStreamHooks) (llm.Message, string, llm.Usage, error) {
	for {
		selection, err := hooks.current()
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		var (
			msg     llm.Message
			sr      string
			usage   llm.Usage
			callErr error
		)
		// 同じ provider の安全ウィンドウ内の再試行:非ストリーミング呼び出しは全体成功または全体失敗で、
		// 途中で出力済みの問題がないため、どの一時的失敗もそのまま再試行可能。
		retries, backoffOf := sameProviderRetryPolicy(selection.retry)
		for attempt := 0; ; attempt++ {
			msg, sr, usage, callErr = selection.provider.Complete(ctx, req)
			if callErr != nil && ctx.Err() == nil &&
				attempt < retries && isRetryableStreamError(callErr) {
				backoff := backoffOf(attempt)
				log.Printf("[task-llm] task %s の非ストリーミング呼び出しに失敗しました。%v 後に同じ provider で再試行します (%d/%d): %v",
					taskID, backoff, attempt+1, retries, callErr)
				if sleepCtx(ctx, backoff) {
					break // バックオフ中に ctx キャンセル → 再試行停止
				}
				continue
			}
			break
		}
		if callErr == nil {
			return msg, sr, usage, nil
		}
		// profileID=0: 明示チェーンはクリア済み;割当枠以外のエラー:そのまま渡す。どちらもタスクの failover 状態は変更しない。
		if selection.profileID == 0 || !isQuotaExhaustedError(callErr) {
			return llm.Message{}, "", llm.Usage{}, callErr
		}
		transition, markErr := hooks.exhaust(selection, callErr)
		if markErr != nil {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("mark profile quota exhausted after %v: %w", callErr, markErr)
		}
		if transition.Advanced && !transition.Stale && hooks.transition != nil {
			hooks.transition(selection, transition, callErr)
		}
		if !transition.Stale && transition.NextProfileID == nil {
			return llm.Message{}, "", llm.Usage{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: callErr}
		}
		// 呼び出し元へ出力を一切渡していないため、次の profile で同じ論理リクエストを再実行しても安全。
	}
}

func streamTaskLLM(ctx context.Context, taskID string, req llm.CompletionRequest, hooks taskLLMStreamHooks) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for {
			selection, err := hooks.current()
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			committed := false
			var pending []llm.StreamEvent
			var streamErr error
			retries, backoffOf := sameProviderRetryPolicy(selection.retry)
			// 同じ provider の安全ウィンドウ内の再試行:committed 前(呼び出し元へ出力を一切渡していない)の
			// 一時的失敗はそのまま再実行でき、モデル出力やツール実行を重複しない。committed 後、
			// ctx キャンセル、または確定的/割当枠エラーでは抜け、下の既存の透過/フェイルオーバーロジックに委ねる。
			for attempt := 0; ; attempt++ {
				committed = false
				pending = nil
				streamErr = nil
				for event, err := range selection.provider.Stream(ctx, req) {
					if err != nil {
						streamErr = err
						break
					}
					if !committed && !streamEventCommitsOutput(event) {
						pending = append(pending, event)
						continue
					}
					if !committed {
						for _, buffered := range pending {
							if !yield(buffered, nil) {
								return
							}
						}
						pending = nil
						committed = true
					}
					if !yield(event, nil) {
						return
					}
				}
				if streamErr != nil && !committed && ctx.Err() == nil &&
					attempt < retries && isRetryableStreamError(streamErr) {
					backoff := backoffOf(attempt)
					log.Printf("[task-llm] task %s の確定前のストリームに失敗しました。%v 後に同じ provider で再試行します (%d/%d): %v",
						taskID, backoff, attempt+1, retries, streamErr)
					if sleepCtx(ctx, backoff) {
						break // バックオフ中に ctx キャンセル → 再試行停止
					}
					continue
				}
				break
			}
			if streamErr == nil {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				return
			}
			// profileID=0 means the explicit chain was cleared while this stable task
			// bundle was still in use. Agent/global fallback errors follow the legacy
			// behavior and never mutate task failover state.
			if selection.profileID == 0 || !isQuotaExhaustedError(streamErr) {
				for _, buffered := range pending {
					if !yield(buffered, nil) {
						return
					}
				}
				yield(llm.StreamEvent{}, streamErr)
				return
			}
			transition, markErr := hooks.exhaust(selection, streamErr)
			if markErr != nil {
				cause := fmt.Errorf("mark profile quota exhausted after %v: %w", streamErr, markErr)
				if committed {
					// Output may already have driven tool execution. Report the persistence
					// failure, but classify it as router-handled so the worker does not
					// replay the entire intent and duplicate those side effects.
					yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, cause: cause})
				} else {
					yield(llm.StreamEvent{}, cause)
				}
				return
			}
			if transition.Advanced && !transition.Stale && hooks.transition != nil {
				hooks.transition(selection, transition, streamErr)
			}
			if committed || (!transition.Stale && transition.NextProfileID == nil) {
				yield(llm.StreamEvent{}, &taskLLMError{taskID: taskID, chainExhausted: transition.ChainExhausted, cause: streamErr})
				return
			}
			// No event reached the caller, so replaying the same logical request on
			// the next profile cannot duplicate model output or tool execution.
		}
	}
}

func streamEventCommitsOutput(event llm.StreamEvent) bool {
	switch event.Type {
	case llm.SETextDelta, llm.SEThinkingDelta, llm.SEToolInputJSON:
		return event.Text != ""
	case llm.SEToolUseStart, llm.SEMessageDelta, llm.SEMessageStop:
		return true
	default:
		return false
	}
}

// コミット前の安全ウィンドウ内で、同じ provider に対する【デフォルト】再試行回数。SDK の doStream は接続
// 段階(200 取得前)のみ再試行し、ストリーム開始後の途中切断 / overloaded / ストリーム内の 429 などの一時障害は直接
// model_error として伝播し、再試行はゼロ。token を1つも呼び出し元へ渡していなければ(!committed)、
// 完全に同じリクエストの再実行でモデル出力やツールの副作用は重複しないため、ここで同じ provider のバックオフ再試行を1層補い、
// 意図全体の再実行前にこの種の揺らぎを吸収する。LLM 設定の再試行上書き/グローバル再試行ポリシーで変更可能。
const sameProviderStreamRetries = 2

// sameProviderRetryBackoff は第 attempt 回の再試行前の【デフォルト】バックオフ(0.5s、1s…、上限 4s)で、
// SDK の指数段階と同じ形式だが上限を小さくし、worker の終了処理/キャンセル応答を遅らせないようにする。
// 変数で公開し、テストでバックオフをゼロにしやすくする。
var sameProviderRetryBackoff = func(attempt int) time.Duration {
	return min(500*time.Millisecond*(1<<attempt), 4*time.Second)
}

// sameProviderRetryPolicy は今回使う同じ provider の再試行パラメーターを解決:設定に回数があれば
// それを使用し(負数 = この層の再試行を無効)、間隔があれば指数バックオフを固定間隔に変え、両方未設定なら
// 設定可能にする前とバイト単位で一致する。
func sameProviderRetryPolicy(r agent.RetryConfig) (retries int, backoff func(int) time.Duration) {
	retries, backoff = sameProviderStreamRetries, sameProviderRetryBackoff
	if r.StreamAttempts != 0 {
		retries = max(r.StreamAttempts, 0)
	}
	if r.StreamInterval > 0 {
		d := r.StreamInterval
		backoff = func(int) time.Duration { return d }
	}
	return retries, backoff
}

// isRetryableStreamError は「コミット前のストリーム失敗」を同じ provider で再実行する価値があるか判定する。
// 一時的な伝送切断 / 提供元の過負荷 / レート制限は自然に回復し、安全に再試行可能；ただし以下の3種類は再試行しない:
//   - 割当枠の枯渇:profile のフェイルオーバーに委ね、ここで再試行を無駄に消費しない
//   - コンテキスト過長:同じリクエストを再実行しても無効で、harness の reactive 圧縮に委ねる
//   - 4xx の確定的拒否(400/401/403/404/422):どの provider でも同様に失敗する
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	if isQuotaExhaustedError(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "too long") || strings.Contains(s, "context length") ||
		strings.Contains(s, "context_length") || strings.Contains(s, "maximum context") ||
		strings.Contains(s, "status 413") {
		return false
	}
	for _, code := range []string{"status 400", "status 401", "status 403", "status 404", "status 422"} {
		if strings.Contains(s, code) {
			return false
		}
	}
	// その他(伝送の reset/EOF/timeout、408/429/5xx、ストリーム内の error イベントで anthropic の
	// overloaded_error など)は一律一時的と見なし、再試行を許可。
	return true
}

// CompactionWindow mirrors current()'s precedence so the context window always
// matches the provider the role will actually stream on.
func (r *taskLLMRuntime) CompactionWindow() int {
	if _, cfg, ok := r.s.agentBindingProvider(r.agentKey); ok {
		return cfg.CompactionWindow()
	}
	taskNum, err := parseTaskID(r.taskID)
	if err != nil {
		return (agent.Config{}).CompactionWindow()
	}
	chain, err := r.s.m.pg.TaskLLMProfiles(taskNum)
	if err != nil {
		return (agent.Config{}).CompactionWindow()
	}
	if len(chain) == 0 {
		if _, cfg, ok := r.s.globalProvider(); ok {
			return cfg.CompactionWindow()
		}
		return (agent.Config{}).CompactionWindow()
	}
	minimum := 0
	for _, entry := range chain {
		if cfg, ok := r.s.loadProfileConfig(entry.ProfileID); ok {
			window := cfg.CompactionWindow()
			if minimum == 0 || window < minimum {
				minimum = window
			}
		}
	}
	if minimum == 0 {
		return (agent.Config{}).CompactionWindow()
	}
	return minimum
}

// agentBindingProvider resolves the profile a role is explicitly bound to
// (agents.llm_profile_id) — the highest-precedence level for task agents. ok=false
// when the role has no binding or the bound profile no longer builds, so callers
// fall through to the task chain. A bound profile stays exclusive unless
// llm_pool_bind_fallback is on, which is what poolForBinding encodes.
func (s *Server) agentBindingProvider(agentKey string) (llm.Provider, agent.Config, bool) {
	id := s.effectiveProfileForAgent(agentKey, nil)
	if id == nil {
		return nil, agent.Config{}, false
	}
	prov, cfg, ok := s.providerForProfile(*id)
	if !ok {
		return nil, agent.Config{}, false
	}
	return s.poolForBinding(*id, prov, cfg), cfg, true
}

// globalProvider returns the process-wide provider (persisted active profile or
// environment config) — the last resort once a role has neither a binding nor a
// task chain.
func (s *Server) globalProvider() (llm.Provider, agent.Config, bool) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if !s.llmOn || s.llmProv == nil {
		return nil, agent.Config{}, false
	}
	return s.llmProv, s.llmCfg, true
}

// taskRuntimeAvailable reports whether every listed role can resolve a provider
// under the runtime precedence in current(): the role's own binding first, then
// the task chain, then global. An exhausted chain is a hard stop for unbound
// roles rather than a silent fall through to global — same as current().
func (s *Server) taskRuntimeAvailable(t *Task, agentKeys ...string) bool {
	if t == nil || len(agentKeys) == 0 {
		return false
	}
	state := t.llmStateSnapshot()
	unboundReady := false
	if len(state.ProfileIDs) > 0 {
		if state.ActiveID != nil {
			_, _, unboundReady = s.providerForProfile(*state.ActiveID)
		}
	} else {
		_, _, unboundReady = s.globalProvider()
	}
	for _, key := range agentKeys {
		if _, _, ok := s.agentBindingProvider(key); ok {
			continue
		}
		if !unboundReady {
			return false
		}
	}
	return true
}

func (s *Server) invalidateTaskAgents() {
	s.taskAgentMu.Lock()
	s.taskAgents = map[string]*taskAgentBundle{}
	s.taskAgentMu.Unlock()
}

func (s *Server) agentsForTask(t *Task) *taskAgentBundle {
	s.taskAgentMu.Lock()
	defer s.taskAgentMu.Unlock()
	if bundle := s.taskAgents[t.ID]; bundle != nil {
		return bundle
	}
	goalRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "goals"}
	plannerRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "planner"}
	workerRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "worker"}
	mainRuntime := &taskLLMRuntime{s: s, taskID: t.ID, agentKey: "mainagent"}
	tx := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	window := workerRuntime.CompactionWindow()
	wk := agent.NewWorker(workerRuntime, "task-router", s.m.dir, tx, window, s.agentMaxTurns("worker"))
	wk.SetFindingRecorder(s.evidenceStore())
	wk.SetCompactionWindowResolver(workerRuntime.CompactionWindow)
	wk.SetNonStreaming(workerRuntime.nonStreaming) // タスクの現在有効な profile のストリーミングスイッチに従う(毎回読む)
	wk.SetMaxTokens(workerRuntime.maxTokens)       // 同上、出力上限も現在有効な profile に従う
	wk.SetNoaEnabled(s.m.NoaCompactionEnabled)     // 実験機能:noa のコンテキスト圧縮(プラットフォーム単位のスイッチ、run ごとに読む)
	wk.SetRunTimeout(time.Duration(s.agentRunSeconds("worker")) * time.Second)
	wk.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	wk.SetWebSearch(s.webSearchFor("worker"))
	wk.SetConstraintInject(s.constraintInjectWorker) // worker に操作制約を注入(設定可能、デフォルト有効)
	pl := agent.NewPlanner(plannerRuntime, "task-router", s.m.dir, tx, plannerRuntime.CompactionWindow(), s.agentMaxTurns("planner"))
	pl.SetFindingRecorder(s.evidenceStore())
	pl.SetCompactionWindowResolver(plannerRuntime.CompactionWindow)
	pl.SetNonStreaming(plannerRuntime.nonStreaming)
	pl.SetMaxTokens(plannerRuntime.maxTokens)
	pl.SetNoaEnabled(s.m.NoaCompactionEnabled) // 実験機能:noa のコンテキスト圧縮(プラットフォーム単位のスイッチ、run ごとに読む)
	pl.SetKillWork(s.engine.KillWork)
	pl.SetSteerWork(s.engine.SteerWork)
	pl.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	pl.SetWebSearch(s.webSearchFor("planner"))
	pl.SetConstraintInject(s.constraintInjectPlanner) // planner に操作制約を注入(設定可能、デフォルト有効)
	// cold-digest §7: cold ノードのバックグラウンド圧縮。エンジンが正式なリゾルバー経由で実際に駆動するのは、この per-task planner
	// (agentsForTask)なので、Compactor はここに接続する。タスク経路の planner provider(§4:agent と
	// 同じモデルで、タスクの LLM チェーンに従って解決)を使用し、圧縮は Complete で body を一括生成する。
	pl.SetCompactor(agent.NewCompactor(plannerRuntime, "task-router"))
	main := agent.NewMainAgent(mainRuntime, "task-router", s.m.dir, tx, mainRuntime.CompactionWindow(), s.agentMaxTurns("mainagent"))
	main.SetFindingRecorder(s.evidenceStore())
	main.SetCompactionWindowResolver(mainRuntime.CompactionWindow)
	main.SetNonStreaming(mainRuntime.nonStreaming)
	main.SetMaxTokens(mainRuntime.maxTokens)
	main.SetNoaEnabled(s.m.NoaCompactionEnabled) // 実験機能:noa のコンテキスト圧縮(プラットフォーム単位のスイッチ、run ごとに読む)
	main.SetProxy(s.m.ProxyAddr(), s.m.ProxyCACert())
	main.SetWebSearch(s.webSearchFor("mainagent"))
	main.SetSteerWork(s.engine.SteerWork) // steer_work：人が実行中の work をリアルタイムで修正
	bundle := &taskAgentBundle{
		runtime: goalRuntime, plannerRuntime: plannerRuntime, workerRuntime: workerRuntime,
		mainRuntime: mainRuntime, pl: pl, wk: wk, main: main,
	}
	s.taskAgents[t.ID] = bundle
	return bundle
}

func (s *Server) syncTaskLLMState(pt *db.Task) {
	if pt == nil {
		return
	}
	id := fmt.Sprintf("%d", pt.ID)
	s.m.mu.Lock()
	if task := s.m.tasks[id]; task != nil {
		task.setLLMState(pt.LLMProfileID, pt.ActiveLLMProfileID, pt.LLMProfileIDs, pt.LLMChainRevision, pt.LLMFailoverState, pt.LLMFailoverReason)
	}
	s.m.mu.Unlock()
}

func (s *Server) emitTaskLLMTransition(t *Task, transition db.TaskLLMTransition, cause error) {
	if t == nil {
		return
	}
	previous := s.llmAuditProfile(transition.PreviousProfileID)
	var next *llmAuditProfile
	if transition.NextProfileID != nil {
		next = s.llmAuditProfile(*transition.NextProfileID)
	}
	mode := "automatic"
	kind := "llm_switch"
	summary := fmt.Sprintf("%s の利用枠が不足しています", llmAuditProfileLabel(previous))
	if transition.NextProfileID != nil {
		summary += fmt.Sprintf("。次回の呼び出しから %s に切り替えます", llmAuditProfileLabel(next))
	} else {
		mode = "exhausted"
		kind = "llm_failover"
		summary += "。設定チェーンを使い切りました"
	}
	metadata, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
		Mode: mode, Reason: cause.Error(), Previous: previous, Next: next,
	}})
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: kind, IsError: transition.ChainExhausted, Summary: summary, Detail: cause.Error(), Metadata: metadata})
	log.Printf("[llm-failover] task %s: %s", t.ID, summary)
}

func (s *Server) llmAuditProfile(id int64) *llmAuditProfile {
	if id <= 0 || s.m == nil || s.m.pg == nil {
		return nil
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p == nil {
		return &llmAuditProfile{ID: id, Name: fmt.Sprintf("設定 #%d", id)}
	}
	return &llmAuditProfile{ID: p.ID, Name: p.Name, Format: p.Format, Model: p.Model}
}

func llmAuditProfileLabel(profile *llmAuditProfile) string {
	if profile == nil {
		return "デフォルト設定"
	}
	name := profile.Name
	if name == "" {
		name = fmt.Sprintf("設定 #%d", profile.ID)
	}
	detail := []string{}
	if profile.Format != "" {
		detail = append(detail, profile.Format)
	}
	if profile.Model != "" {
		detail = append(detail, profile.Model)
	}
	if len(detail) == 0 {
		return name
	}
	return fmt.Sprintf("%s（%s）", name, strings.Join(detail, " / "))
}

func sameOptionalID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *Server) emitManualTaskLLMSwitch(t *Task, previousID, nextID *int64) db.Activity {
	previous := (*llmAuditProfile)(nil)
	next := (*llmAuditProfile)(nil)
	if previousID != nil {
		previous = s.llmAuditProfile(*previousID)
	}
	if nextID != nil {
		next = s.llmAuditProfile(*nextID)
	}
	summary := fmt.Sprintf("ユーザーがタスクの LLM を %s から %s に切り替えました", llmAuditProfileLabel(previous), llmAuditProfileLabel(next))
	metadata, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
		Mode: "manual", Reason: "ユーザーがタスクの LLM を手動で切り替えました", Previous: previous, Next: next,
	}})
	return s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "llm_switch", Summary: summary, Detail: summary, Metadata: metadata})
}
