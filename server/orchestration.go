package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// このファイルは P2「タスク間編成ツール群」（docs/跑分编排 §2 P2）を実装します。host ツールであり、
// Manager（任意タスクの Store）、Engine（一時停止）、タスク作成処理へアクセスするため server 層に置きます。
// 読取ツールは「既存の per-task ツール」を対象タスクの store へ向け直します（一時的な ToolSet を作り、
// 対応ツールを Call）。同じ処理を再利用し、制御系（spawn/pause）は Manager/Engine を直接呼びます。
// トラフィックツール同様、tools テーブルへ seed し、agent 別に関連付けます（編成 agent だけに付ければ他からは見えない）。

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // プラットフォーム操作ツール（skill/ツール/MCP の作成変更、Auto 用）
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 読み込みに失敗しました: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id は必須です"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task が存在しません: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 汎用の起動通知（専用コールバックのない書込操作が使用、読取ツールは no-op）
	tsx.SetNotifyHint(t.NotifyHint) // add_hint →「人が N 件の戦略ヒントを追加：…」を記録し、planner を起動
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"すべてのタスクの id・説明・目標・状態・実行時間・親タスク・LLM 設定を一覧表示します。オーケストレーション agentが全体の状況、停滞しているタスク、各タスクの使用 LLM を把握するために使用します。実行時間は秒単位で、実行中は作成から現在まで、終了済みは作成から最終活動までです。llm_profile はタスクの planner/worker が使う設定名で、(アクティブ設定) はグローバルのアクティブ設定に従うことを表します。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(アクティブ設定)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(削除済み)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"利用可能な LLM 設定（profile）の id・名前・モデル・形式・現在のアクティブ設定かどうかを一覧表示します。spawn_task の llm_profile_id に id を渡すと、子タスク専用の LLM を指定できます（例: 偵察には低コストのモデル、エクスプロイトには高性能なモデル）。API Keyは含みません。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"子タスクを作成して探索エンジンを起動し、task_id を返します。個別の課題や目標を独立したタスクとして割り当てるために使用します。parent_ref は任意で、現在のオーケストレーションに関連する親タスクの id を指定すると親子関係を設定できます。",
		objSchema(map[string]any{
			"description":            strParam("タスクの説明（短いタイトル）"),
			"goal":                   strParam("タスクの目標（達成すること）"),
			"parent_ref":             strParam("任意: 親タスクの id（親子関係を設定）"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("任意: 読み取り専用で継承する元タスクの id 一覧（最大%d件）。子タスクは、これらのタスクで判明した資産や結論を出発点として参照できます。親子関係だけを表す parent_ref と異なり、内容を継承します。", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "任意: この子タスクの planner/worker が使う LLM 設定の id（list_llm_profiles を参照）。省略時は親タスクの設定を継承し、それもなければグローバルのアクティブ設定を使用します"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "任意: タスクのタイムアウト（秒）。時間に達すると終了処理を行い、最終状態 timeout になります。省略または 0 は時間制限なしです"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "任意: planner のハートビート間隔（秒）。前回の計画終了またはタスク開始から、この時間だけトリガーがなければ計画を実行します。デッドロックの回避と実行中のworkerの監督に使用します。省略または 0 は既定の 600 秒（10min）です"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "任意: 単純なタスクで有効にすると、作成時に説明と目標から初期インテントを1件割り当て、workerが最初の planner を待たずにテストを開始します。既定の false では、通常どおり計画してから実行します。"},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "無題のタスク"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal は必須です"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 読取専用の元タスク：件数上限と各 id の妥当性/重複排除/存在を検証し、HTTP タスク作成と同じ規則にする。
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("関連タスクは最大%d件です", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("関連タスク id が無効か、重複しています"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("関連タスク #%d が存在しません", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 設定 #%d が存在しないか、API Key が未設定です", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// 作成後の共通処理。HTTP 作成（server.go createTask）と同じ launchTask を再利用：
			// seed + 背後で表示可能な目標分解（第0回/LLMステップ/各goal）+ engine.Run。
			// seed_first_intent は既定 false（先に計画して実行）。単純なタスクでは有効にして work を直接一件実行できます。
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "指定したタスクを一時停止し、planner/worker のループを停止します。",
		objSchema(map[string]any{"task_id": strParam("一時停止するタスクの id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task が存在しません: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "task_id で指定したタスクの探索グラフの概要を読み取ります（graph_overview と同じく、資産数・frontier・検出結果・カバレッジなどを含みます）。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "task_id で指定したタスクの確認済み脆弱性を読み取ります。flag/PoC を含み、各項目に id/task_id/intent_id/vulnclass/severity・要約・状態を返します。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "指定したタスクに戦略ヒントを追加します。そのタスクの planner が次回のインテント生成時に読み取ります。\n"+
		"★一括登録を優先: 複数のヒントは hints 配列でまとめて送信してください。返される ids 配列は hints と同じ件数・順序で、失敗項目の id は 0 です。1件だけなら hints を省略し、最上位の text を指定します。",
		objSchema(map[string]any{
			"task_id":      strParam("タスク id"),
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先】ヒントの配列。各要素のフィールドは最上位と同じです（text/asset_ids/traffic_refs）。", "items": objSchema(map[string]any{"text": strParam("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[1件のみ] ヒントの内容"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関連付ける資産の id（任意。0件・1件・複数件を指定でき、このタスク内の資産 id を使用します）"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"指定したタスクの work（インテント）の実行過程を確認します。get_task_worker_trace(task_id, intent_id) でステップの要約を取得し、step_ids=[...] を追加して該当ステップの全文を取得します。1回に最大5件で、それ以上指定した場合は先頭5件だけを返します。",
		objSchema(map[string]any{
			"task_id":   strParam("タスク id"),
			"intent_id": map[string]any{"type": "integer", "description": "インテント id（このタスクの work）"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "任意: 全文を取得するステップの id（1回に最大5件。それ以上指定した場合は先頭5件を返し、残りは omitted_step_ids に列挙します）"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "指定したタスクで実行した work（インテント）と、それぞれのステップ数を一覧表示します。詳しく確認する work を選び、get_task_worker_trace で内容を取得するために使用します。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "指定したタスクの全 work の実行過程をキーワードで検索し、一致したステップの要約と intent_id を返します。",
		objSchema(map[string]any{"task_id": strParam("タスク id"), "q": strParam("検索キーワード")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"指定したタスクの探索グラフノードの全文を読み取ります（検出結果・事実・インテント・目標の要約と、詳細・証拠・PoC）。id は探索ノードの id です（report_finding の戻り値や list_task_findings の id）。脆弱性レポートを作成する前に、これを使って証拠全体を取得してください。",
		objSchema(map[string]any{
			"task_id": strParam("タスク id"),
			"id":      map[string]any{"type": "integer", "description": "探索グラフのノード id（資産 id ではありません）"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"登録済みの脆弱性の詳細レポートを作成・更新します。Markdown の全文で既存の内容を上書きします。finding_id には report_finding が返す id（\"finding recorded: <id>\" の数字）を指定してください。レポートには脆弱性の概要、影響とリスク、再現手順、証拠 / PoC、修正案を含めることを推奨します。",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "対象の脆弱性 id（report_finding が返す id）"},
			"report":           strParam("詳細レポートの全文（Markdown 形式）"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic が返す証拠の version。新しい証拠の変更を古いレポートで上書きしないために使用します"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「数値または数値文字列」の解析を再利用
			if nodeID <= 0 {
				return actool.Errorf("finding_id が無効です"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d に対応する脆弱性レコードが見つかりません(先に report_finding で登録してください)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent（元から
	// プラットフォームの操作用）。SeedTool は初回挿入で反映し、旧 DB で seed 済みの行は seedAutoDefaultBindings で関連付けを補います。
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces を worker の既定から解除（一度だけ）
	s.seedWorkerReadbackRebind()  // 旧移行の誤削除を修正し、search_all_worker_traces/get_worker_trace/node_detail を worker へ戻す（一度だけ）
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals の指示に「操作制約の抽出」を追加 → 旧 DB に新既定版を追加（一度だけ）
	s.reseedMainAgentPrompt()         // mainagent の指示に「達成後の add_intent で目標登録を確認」を追加（一度だけ）
	s.reseedPlannerPrompt()           // planner の指示：「0 インテント」の正当条件を書き直し、定量的な達成条件を確認（一度だけ）
	s.reseedWorkerPrompt()            // worker の指示：否定結論の証拠要件を追加（一度だけ）
	s.seedReporterAgent()             // 「レポート作成」agent + ツール関連付け + finding トリガーを用意（一度だけ）
	s.upgradeReporterTriggerMessage() // 旧 DB の移行補完：reporter に evidence_version を返させる（一度だけ）
	s.seedFindingTrafficTools()       // 任意の証拠引数と読取専用の証拠ツールを追加し、ユーザー設定を保持
	s.seedFindingWorkflowTools()
	// 注意：pentest の既定ツールは移行不要。BuiltinToolSeeds が新規初期化時に、
	// list_assets/insert_assets/report_finding/list_findings/list_companies と
	// pentest をまとめて seed 済みです（まだ旧 DB がないため移行しない）。
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task の llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 一部の組み込み agent ツールもコードの既定へ更新：
	//   - goal_met：旧 DB で seed された説明は「今回の計画を終了」と誤解させ、planner が
	//     空のターンを終了する手段と扱い、開始直後にタスク全体の完了を誤判定していました。
	//   - insert_assets：related 引数を追加（現在のタスクとの関連とカバレッジへの算入を指定）。
	//     SeedTool は初回挿入only なので、旧 DB の seed 済み schema にそのままでは追加されません。
	//   - list_facts：ページ分割へ変更し limit/before/q 引数を追加。旧 DB の seed 済みの空 schema のままでは、
	//     管理画面に「引数なし」と表示され、モデルにも引数の説明が届きません。
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform ツールの schema をコードの既定値に更新しました(一度のみ)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met と planner の関連付け解除に失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt は goals の目標分解プロンプトを【現在のコードの既定】へ更新します。本文に
// 「先に操作制約（set_constraints）を抽出してから分解」を追加しましたが、SeedPromptIfEmpty は初回挿入only で、旧 DB の
// version 1 には届きません。版管理で【新しい版を追加】して切り替えます（ResetPromptToDefault）。
// 旧版は履歴に保持し、カスタマイズしていたユーザーも復元できます。settings flag の保護で一度だけ実行し、
// 既定の次回変更時は flag を bump します。新規 DB は不要です（SeedPromptIfEmpty が最新の既定を seed）。
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 新規 DB に agent 行がまだなければ seedPrompts が最新の既定を seed するため、移行不要
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 新規 DB で seedPrompts が最新既定を seed 済みなら現在の版と同じなので、重複版を追加しません。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals のプロンプトの新しい既定値への更新に失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] goals のプロンプトに新しい既定バージョンを追加しました(操作の制約を抽出する手順を追加、一度のみ)")
}

// reseedMainAgentPrompt は mainagent を【現在のコードの既定】へ更新します。「全目標
// 達成後に add_intent で直接追加するとき、正式な目標へ登録するか確認」という指示を追加しましたが、SeedPromptIfEmpty は初回挿入
// only で旧版には届きません。版管理で【新しい版を追加】し、切り替えます（ResetPromptToDefault）。旧版は
// 履歴に保持し、カスタマイズ済みのユーザーも復元できます。settings flag で一度だけ実行。新規 DB は不要
// （SeedPromptIfEmpty が最新既定を seed 済み）。reseedGoalsPrompt と同じ構造です。
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 新規 DB に agent 行がまだなければ seedPrompts が最新既定を seed するため、移行不要
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 新規 DB で seedPrompts が最新既定を seed 済みなら現在の版と同じなので、重複版を追加しません。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent のプロンプトの新しい既定値への更新に失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] mainagent のプロンプトに新しい既定バージョンを追加しました(目標達成後に目標の作成を問い返す指示を追加、一度のみ)")
}

// reseedPlannerPrompt は planner を【現在のコードの既定】へ更新します。本文を簡略化し、「抑制」を
// 重複排除だけに限定。「カバレッジより深さ」「未達成で実行中インテントがなければ必ず作成」を追加し、否定の再検証に上限を設けます。
// 実質的に変更するたびに下の flag（現在 v2）を bump し、旧 DB を再更新します。SeedPromptIfEmpty は初回挿入only のため、旧版へ反映できず、版管理で
// 【新しい版を追加】して切り替えます（ResetPromptToDefault）。旧版は履歴へ保持し、カスタマイズ済みのユーザーも履歴から
// 復元できます。settings flag により一度だけ実行。新規 DB は不要（SeedPromptIfEmpty が最新既定を seed 済み）。
// reseedGoalsPrompt と同じ構造です。
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 新規 DB に agent 行がまだなければ seedPrompts が最新既定を seed するため、移行不要
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 新規 DB で seedPrompts が最新既定を seed 済みなら現在の版と同じなので、重複版を追加しません。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner のプロンプトの新しい既定値への更新に失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] planner のプロンプトに新しい既定バージョンを追加しました(簡潔な構成への整理+抑制を重複排除へ限定+深さの優先+否定の再確認回数の上限、一度のみ)")
}

// reseedWorkerPrompt は worker を【現在のコードの既定】へ更新します。record_fact 節から「否定の結論は
// 観測 + 暫定的な解釈」という一文を削除し、confidence（observed/inferred）を「このインテントの手段を試し切ったか」から分離（計画担当を誤解させやすいため）し、
// facts 配列の分割も「完全に独立し統合不能」という少数の例外だけへ制限します。flag を v3 へ bump して旧 DB を更新。
// SeedPromptIfEmpty は初回挿入only のため、版管理で【新しい版を追加】して切り替え、復元用に旧版を履歴へ残します。
// settings flag で一度だけ実行。新規 DB は不要。reseedGoalsPrompt と同じ構造です。
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 新規 DB に agent 行がまだなければ seedPrompts が最新既定を seed するため、移行不要
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 新規 DB で seedPrompts が最新既定を seed 済みなら現在の版と同じなので、重複版を追加しません。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker のプロンプトの新しい既定値への更新に失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] worker のプロンプトに新しい既定バージョンを追加しました(コンテキスト確認の部分を list_assets/list_findings に絞り、list_facts/node_detail/asset_neighbors を削除、一度のみ)")
}

// reporterToolCallMessage は必ず get_finding_traffic を一回読んでから報告を書くよう要求します。
// 読取専用で「取得スイッチに依存しない」ツールなので、自動関連付けが無効でも手動の証拠を読めます。ここを
// 「自動関連付け有効時だけ読む」にすると、既定の無効設定では reporter が evidence_version を渡さず、
// SetFindingReportVersionByNodeID が legacy として -1 を保存します。脆弱性詳細と Markdown 出力には
// 「証拠変更済み、報告更新待ち」が残り続け、UI には解消する入口がなくなります。
const reporterToolCallMessage = "上記で report_finding により脆弱性が1件登録されました。戻り値の JSON から finding_id（独立した脆弱性レコード ID）と finding_node_id（探索ノード ID）を読み取り、" +
	"まず get_finding_traffic(finding_id) で現在の証拠一覧とその version を読み取ってください（空の一覧も正常です。そのままレポートを作成してください）。" +
	"実行時の指針で自動関連付けが有効になっている場合は、読み取る前に今回の脆弱性のトラフィックを確認して関連付けてください。ノードの詳細には finding_node_id を使ってください。" +
	"最後に update_finding_report(finding_id=finding_node_id, report, evidence_version=実際に読み取ったバージョン) を呼び出して保存してください。" +
	"evidence_version は必ず渡してください。渡さないと、レポートに更新待ちの表示が残り続けます。2種類の番号を混同しないでください。"

// 旧トリガーメッセージ（0.3.8 以前）。完全一致する記録だけを移行で更新し、ユーザー変更済みは保持。
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage は旧 DB の既定の reporter トリガーメッセージを更新します。
// seedReporterAgent は reporter_agent_seed_v1 で保護し、新規 agent の作成時だけトリガーを書くため、
// 更新した DB には新しい本文が届きません。ツール schema には seedFindingTrafficTools が
// evidence_version を補いますが、reporter が使う指示がないためです。一度だけ、未変更の既定だけを更新します。
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 一度だけ試行
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] トリガーの読み込みに失敗しました: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // ユーザー変更済み、または finding トリガーでなければ保持。
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 起動メッセージの更新に失敗しました: %v", err)
			return
		}
		log.Printf("[reporter] 起動メッセージを evidence_version の読み取りと返却を求める内容に更新しました")
	}
}

// seedReporterAgent は「レポート作成」のカスタム agent を用意（builtin=false、UI で編集/削除可能）。
// update_finding_report とタスク照会ツールを関連付け、「report_finding の呼び出し時に起動」する
// トリガーを設定し、脆弱性の登録ごとに詳細報告を書かせます。settings flag で一度だけ実行し、ユーザー削除後は再作成しません。
// 依存：orchestration ツールはこの関数の上で SeedTool により DB 保存済みのため、関連付け可能。
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成否にかかわらず一度だけ試行

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // ユーザーが手動で key を使用済みなら上書きしない
	}
	a, err := s.m.pg.CreateAgent("reporter", "レポートの作成",
		"脆弱性が見つかると自動で起動し、証拠と実行過程を調べて詳細な Markdown レポートを作成・保存します。")
	if err != nil {
		log.Printf("[reporter] agent の作成に失敗しました: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt に失敗しました: %v", err)
	}
	// トリガー実行方針：parallel + none。一脆弱性一報告で、複数 finding を並行して個別に作成。
	// merge は必ず none にします。既定の all では一群の finding が一実行へ統合され、並行処理の意味がなくなります。
	// maxParallel=5：同時に最大 5 報告セッションとし、LLM 呼び出しの瞬間的な集中を防ぎます。
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 起動時の実行ポリシーの設定に失敗しました: %v", err)
	}
	// 必要なツールを関連付け：報告作成 + 証拠/実行過程/状況の読取。
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] ツールの関連付けに失敗しました: %v", err)
	}
	// トリガー：report_finding の呼び出し時に起動（ツールは "finding recorded: <id>" と finding_id を返し、
	// タスク id も起動メッセージへ含めます）。
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] トリガーの作成に失敗しました: %v", err)
	}
	log.Printf("[reporter] 「レポートの作成」agent + finding トリガーを初期設定しました")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding の既定の関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding の既定の関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets の既定の関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // 既定の関連付けを worker→planner へ切り替え
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope の既定の関連付けに失敗しました: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope の関連付けの解除に失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s と worker の関連付け解除に失敗しました: %v", k, err)
			return // エラー時は flag を保存せず、次の起動で再試行
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2：node_detail を追加
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 振り返り/詳細ツールの追加の関連付けに失敗しました: %v", err)
		return // エラー時は flag を保存せず、次の起動で再試行
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3：旧資産ツール名を置換し、insert_assets/add_company_scope を追加
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 資産ツール：Auto によるプラットフォーム操作では資産の確認/登録や企業範囲の管理がよく必要です。
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 既定の関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
