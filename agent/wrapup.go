package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 終了プロンプト(wrap-up / settlement prompt):agent が【ステップ数上限(MaxTurns)】または
// 【タイムアウト(run_seconds/MaxDuration)】で終了すると、SDK の settlement 段階がこのプロンプトを注入し、
// agent に認識済みだが未書き戻しの内容を先に保存させ、その後1文の要約を出力させ、未完了を防ぐ。
//
// 各 agent の終了プロンプトは管理画面で必要に応じて上書き可能(agents.wrapup_prompt に保存)で、空ならここの
// 組み込みデフォルトを使う。【プロンプト本文】のみ編集可能で、無効にするツールや終了段階のラウンド予算はコードの固定ポリシー。

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 組み込みのデフォルト終了プロンプトを agent key で索引化。worker は従来ハードコードされていた settleWrapUpPrompt を再利用し、
// (worker.go で定義)、planner/mainagent は各々1版を持つ;未一致(カスタム agent)は汎用のフォールバックを使用。
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 各 agent の終了段階【自身】のラウンド予算の組み込みデフォルト(管理画面で >0 に上書き可能)。
// いずれも 10 ラウンドを与え、終了段階で保存に十分なステップを確保する。未一致は genericWrapupTurns を使用。
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "今回の計画ラウンドのステップ数をまもなく使い切ります。終了するのは【このラウンドだけ】であり、タスクの終了ではありません。システムはその後も状況の変化に応じて再び起動し、計画を続けます。ここで計画全体を終える必要はありません。このラウンドを無駄にしないよう、すでに判断した結論を記録してください。ただし、【終了処理のために無理にインテントを作らないでください】（このラウンドでインテント0件でも正常です）。(一)【今割り当てるべき】探索方向をすでに判断した場合は、add_intent の1回の呼び出しでまとめて登録してください（決めたものは登録してください）。(2) 検出結果 / 事実によって達成が証明された目標は、prove_goal で met とし、判定漏れを防いでください。(3) 段階ごとに進める必要のある直列の悪用経路を特定した場合は、次回の起動時に割り当てを続けられるよう TodoWrite で記録してください。完了したらこのラウンドを終了し、要約文の出力は不要です。"

const mainAgentWrapUpDefault = "ステップ数をまもなく使い切り、今回のやり取りが終了します。新たな探索 / 操作を開始しないでください。現在の進捗、重要な結論、推奨する次の手順を、**独立したプレーンテキストの一文で**ユーザーに要約してください。"

const genericWrapUpDefault = "予算を使い切るため、まもなく終了します。まず、完了済みで未保存の結果を書き戻し、その後、実施した作業と重要な結論を**独立したプレーンテキストの一文で**要約してください（この一文を今回の実行結果として表示します）。"

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- タスク単位のタイムアウト終了プロンプト（docs/任务级超时与收尾设计.md を参照）----------
//
// per-run 終了プロンプトとは【別系統】：per-run は"今回の run の予算を使い切った"で、タスクのタイムアウトは
// "タスク全体が期限に達し、まもなく終了する"。意味はしばしば逆になる（特に planner：per-run は"止めずに計画を続ける"、
// タスクのタイムアウトは"期限なので計画を止め、最終判定する"）。worker/planner のみ設定。

// WrapupTaskTimeoutOverride / …TurnsOverride：タスクのタイムアウト終了プロンプトとラウンド数の DB 上書き
// （agents.task_timeout_wrapup_prompt / _max_turns に wire、worker/planner のみ）。
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**タスク全体の制限時間に達したため、まもなく終了します**（今回の run の予算ではなく、探索全体の制限時間です）。これが最後の機会です。(1) 特定済みで未保存の内容を【すべて】保存してください。新しい資産は insert_assets、探索の結論 / 事実は record_fact、確認済みの脆弱性は report_finding を使用します。(2) 新しいコマンド / 調査を開始しないでください。(3) **最後に、独立したプレーンテキストの一文で**、このインテントの重要な結論を要約してください。"

const plannerTaskTimeoutDefault = "**タスク全体の制限時間に達したため、まもなく終了します**（このラウンドではなく、タスク全体が終了します）。現在の【すべて】の事実と検出結果に基づき、最後に目標を判定してください。証拠によって達成が証明された目標は、判定漏れがないよう prove_goal で met にしてください。**新しいインテントを作成しないでください**（ここで割り当てても実行されません）。判定が終わったら終了し、要約文の出力は不要です。"

// TaskTimeoutWrapupDefault は agent のタスクのタイムアウト用組み込みデフォルト終了プロンプトを返す（管理画面の仮表示/デフォルト復元用）。
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 未設定(mainagent/chat)は空文字列を返す
}

// resolveTaskTimeoutWrapup：DB 上書き(非空) > 組み込みデフォルト。空文字列はその agent にタスクのタイムアウト文がないことを表し、
// （worker/planner 以外）、この場合は呼び出し元が per-run 文に戻すこと。
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // デフォルトでは per-run のラウンド数を継続
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → 今回の run はタスクの deadline で制限：Timeout による終了=タスク期限到達→タスクのタイムアウト文；
//     MaxTurns による終了=制限ウィンドウ内で先にステップ数が尽き、タスクは数分残る→per-run 文に戻す。
//   - clamped=false → タスク期限には余裕：どちらの reason も per-run 文を使用（wrapupSettlement と同じに退化）。
//
// harness に渡す PromptByReason が終了時に【実際】の reason に応じて選び、build 時の不一致を防ぐ。
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // フォールバック(非 clamped 時の両方の reason の値でもある)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // タスク期限到達
				harness.ReasonMaxTurns: perRun, // ステップ数が先に尽き、タスク時間は残っている
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
