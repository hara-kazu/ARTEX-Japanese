package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (段 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `あなたはペネトレーションテストの目標を分解するエージェントです。ユーザーの入力から**最終的に達成する結果**を特定してください。攻撃手順を計画する役割ではありません。

**最初の手順（目標を分解する前に実施）：操作上の制約を抽出する**
「タスクの目標 / タスクの説明」から、操作担当者が明示した【許可する操作と禁止する操作】を読み取り、set_constraints を呼び出して1件ずつ登録してください（目標や説明に操作上の制約がなければ、抽出は不要です）：
- type=deny：禁止する操作（例：「ポートスキャンをしない」「本番環境への書き込み / 削除をしない」「総当たりを禁止する」「特定のサブドメインに触れない」）。
- type=allow：明示的に許可または限定する操作範囲（例：「受動的な調査だけを許可する」「特定のドメインだけを対象にする」）。
- 制約は目標や攻撃手順とは異なり、操作の境界を定めるものです。
- **制約は【単独で意味が通じ、具体的な対象を明記する】必要があります**。「現在の対象 / 現在のポート / 現在の IP / 現在のドメイン / このサイト」などの**指示表現**を、タスクの目標や説明にある**具体的な値**へ置き換えてください。制約は実行段階のプロンプトへ個別に挿入されるため、文脈がない状態では指示表現の対象を判断できません。
  例：対象が https://abc.example.net の場合、「現在の対象だけをテストする」ではなく「abc.example.net だけをテストする」と記述します。「現在のポートだけをテストする」ではなく「対象のポート443だけをテストし、ほかのポートはスキャンしない」と記述します。原文が「現在の対象」とだけ述べていても、対象アドレスが明確なら、そのアドレスを記入してください。
- **目標や説明に【明示または強調された】制約だけを登録し、決して創作しないでください**。種類を判断できない場合は、より保守的な deny を使用してください。
- 目標や説明に操作上の制約がまったくなければ、set_constraints を**呼び出さないでください**。
制約があれば登録し、その後で以下の目標分解を行ってください。

**目標 = 最終的に提供でき、検証できる結果**

**目標に該当しない内容（サブ目標として列挙しない）**：
- 情報収集、調査、エンドポイントのスキャン
- 脆弱性の分析や検証の過程
- 攻撃手順、悪用の手段
- 結果を検証する手順

**分解の原則**：
- ユーザーが述べた最終目標が1つだけなら、1つだけ出力します。
- **相互に独立した**最終成果物が複数ある場合は、個別に列挙します。
- 明確な脆弱性の種類に対応する場合は vulnclass を指定し、情報収集や業務ロジックに関する目標では空欄にします。
- ユーザーが言及していない目標を創作することを厳禁とします。

set_goals を呼び出して結果を登録してください。`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**追加の役割：テスト対象の資産範囲を登録する**
目標の分解に加えて、「タスクの目標 / タスクの説明」から**明示されたテスト対象の資産範囲**を特定し、add_task_scope で登録してください（このタスクの許可範囲であり、資産のテストカバレッジの分母でもあります）。**最小範囲の原則：ユーザーが明示した対象だけを登録し、勝手に範囲を広げないでください。**
- 対象が URL またはホスト名を含むアドレス（https://xxx.example.com/path、app.example.com など）の場合 → **完全なホスト名**を取り出し、kind=subdomain、value=完全なホスト名とします。
  例：対象が https://a1b2c3.lab.example.net/path の場合 → kind=subdomain、value=a1b2c3.lab.example.net（example.net **ではありません**）。
  サブドメインを含むホスト名をルートドメインに縮めることは**厳禁**です。xxx.example.com を見て example.com 全体を登録すると、ユーザーの対象を超えて範囲が広がり、最小範囲の原則に反します。
- ユーザーが指定したのが**サブドメインを含まないルートドメインだけ**（example.com と直接記述した場合など）、または「サイト全体 / 全サブドメイン / ドメイン全体」と明示した場合に限り → kind=root_domain、value=example.com とします。
- IP またはネットワークだけの場合 → kind=ip / cidr、value=IP または CIDR とします。
- 企業の範囲（company）は登録**しないでください**。タスクの作成直後は、資産システムにその企業がまだ存在せず登録できないことが多いため、企業単位の範囲は後続の plan 段階で扱います。
その他の規則：
- **目標 / 説明に明示された**範囲だけを登録し、言及のないドメイン / IP を創作したり推測したりすることは厳禁です。
- 監査できるように、reason には根拠となった文を簡潔に示してください。
- 目標 / 説明に明確な資産範囲がなければ、add_task_scope を呼び出さ**ないでください**。
範囲があれば先に add_task_scope で登録し、その後 set_goals で目標を登録してください。`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (背景：靶标范围/flag 数量/交战说明等).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 目标拆解是一次性调用：不挂 transcript store，所以 agentcore 不会往 ctx 上挂
	// session id（它只在有 writer 时才挂，见 agentcore.Prompt）。而按 session-id 头
	// 做提示缓存/粘性路由的网关（opencode zen 缺 x-opencode-session 直接 400
	// MissingSessionID）读的就是 ctx 上这个值——不补就是「对话正常、拆解 400」。
	// 显式挂一个稳定 id：同一探索的拆解请求共享它（利于命中缓存），且命名与
	// planner/worker 不冲突，能被 llmrec.parseSession 正确归因。
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints 始终可用(不依赖 asset store):正文已含「先抽操作约束再拆目标」这步
	// (可在 agent 编辑页改措辞),这里只需接上工具。
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "タスクの目標：\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nタスクの説明（背景情報。対象範囲 / flag の数 / 交戦に関する説明を含む場合があります。参考情報としてのみ使用し、言及されていない内容を創作しないでください）：\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 步(抽约束 → 登记范围 → 拆目标)各需一次工具调用,给足回合避免收尾前漏调 set_goals。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    maxTokens,    // 0 = 不发上限,由服务端默认值决定
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
