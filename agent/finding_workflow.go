package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**脆弱性の番号に関する規約**：finding_id は独立した脆弱性レコードの ID、finding_node_id は探索ノードの ID です。list_findings / list_task_findings / node_detail / get_task_node_detail の id は従来どおり探索ノードの ID であり、独立した番号は同じ返り値の finding_id から読み取ってください。get_finding_traffic / bind_finding_traffic には独立した finding_id を使用します。従来の update_finding_report の finding_id 引数には、引き続き finding_node_id を渡してください。report_finding の最初の行の数字を証拠ツールに使用したり、番号のエラー後に他の数字を推測したりしないでください。"

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n標準では、レポート Agent が作成前にトラフィックを確認して関連付けます。報告する側は、検証コマンド、重要な出力、既存の実際のトラフィック ID と用途を evidence に残し、レポート Agent が実行記録と照合できるようにしてください。関連付けのために追加で通信記録を調べる必要はありません。明示的な即時関連付けにも対応しており、traffic_refs または evidence_hint_id で確認済みの参照を指定できます。後者はこのタスクの指定 hint に保存された構造化参照を読み取ります。いずれかが無効なら、今回の報告はすべて失敗します。TCP / 通信記録がない場合は、これらの任意引数は不要です。返り値の finding_id と finding_node_id は、それぞれ独立したレコードと探索ノードを表します。"
		case "add_hint", "add_task_hint":
			note = "\n確認済みの脆弱性を引き継ぐ際は、対応するヒントの traffic_refs に、確認済みトラフィックの ID、用途、説明、順序を残してください（一件なら最上位、一括の場合は対応する hints 要素）。text には、その記録が証明する具体的な脆弱性を説明してください。呼び出す側は、文章だけを引き継いで既存のトラフィック参照を捨ててはいけません。未確認の候補は証拠として引き継げません。"
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**トラフィックの証拠の引き継ぎ（任意）**：標準では、脆弱性の保存後、レポート作成前に、レポート Agent が自動関連付けを行います。報告する側は、検証コマンド、重要な出力、既存の実際のトラフィック ID と用途を evidence に残し、タスク内では intent_id を付けて、レポート Agent が経緯を追えるようにしてください。関連付けのために追加で通信記録を調べる必要はありません。Auto / Planner が代わりに報告する場合も、実行者の既存の参照を捨てないでください。add_hint / add_task_hint の traffic_refs で引き継げます。明示的な即時関連付けとして、report_finding の traffic_refs / evidence_hint_id も引き続き使用できます。TCP または通信記録がない場合も通常どおり登録し、ID の推測や、通信記録を補うためだけの再調査はしないでください。"
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nプラットフォームのチャットにタスクのコンテキストがない場合は、report_finding を直接呼び出さず、add_task_hint で既存の対応タスクに引き継いでください。タスク Agent が登録し、list_task_findings で結果を照合してください。"
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n目標を達成済みと判定する前に、今回すでにある証拠の報告 / 引き継ぎを完了してください。証拠の引き継ぎが未完了のまま、脆弱性の文章を登録しただけでタスクを終了したり Worker を取り消したりしないでください。通信記録がない場合に、待機や無理なパケット取得は必要ありません。"
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**レポート作成前のトラフィック自動関連付け（有効）**：今回の起動対象となった脆弱性のトラフィックを確認して関連付け、その後レポートを作成してください。まず report_finding が返す JSON、または get_task_node_detail / list_task_findings から明確な finding_id と finding_node_id を取得します。脆弱性の詳細、対応するインテントの実行記録、既存の証拠一覧を読み取り、報告する側から引き継いだ実際の ID を優先して使用してください。今回の検証が HTTP で、トラフィックツールを利用できる場合は、traffic_search で候補を絞り込み、traffic_get でリクエスト / レスポンスがその脆弱性を実際に裏付けることを一件ずつ確認してください。ドメインと時刻は絞り込みにのみ使用し、帰属を証明するものではありません。確認した証拠を、再現順に bind_finding_traffic(finding_id, traffic_refs) で関連付け、baseline / proof / verification / supporting を選んで用途を説明してください。操作するのは今回の脆弱性だけとし、脆弱性の重複作成や対象の再調査はしないでください。関連付けに成功したら、get_finding_traffic を再度呼び出して最新の version を取得し、必要な本文を読み取り、実際に読み取った version を evidence_version として update_finding_report に渡してください（finding_id 引数には引き続き finding_node_id を使用）。既存の関連付けを重複して追加する必要はありません。TCP、未収集、ツールを利用できない場合、または正確な一致がない場合は自動関連付けを省略し、文章 / コマンドの証拠に基づいて通常どおりレポートを作成し、理由を説明してください。トラフィックを揃えるために推測してはいけません。関連付けに失敗したら成功したと述べず、既存の証拠を保持し、関連付けできなかった理由をレポートに記載してください。"
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "任意。このヒント内の具体的な脆弱性に対応し、確認済みの通信記録への参照。順序を保持します。引き継ぎ後、report_finding に evidence_hint_id を渡すと、これらの参照を引き継げます。", "items": obj(map[string]any{"traffic_id": str("実在する通信記録の ID"), "role": str("baseline / proof / verification / supporting"), "note": str("この通信記録が裏付ける結論")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d はこのタスクのヒントノードである必要があります（継承されたヒントを直接関連付けに使用することはできません）", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
