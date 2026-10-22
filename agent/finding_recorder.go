package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**脆弱性のトラフィックの証拠（任意）**：report_finding で脆弱性を報告する際、閲覧して脆弱性の結論を裏付けると確認した HTTP リクエスト / レスポンスがあれば、traffic_refs で実際の ID を再現順に関連付けられます。ドメインと時刻は候補の絞り込みにのみ使用し、関連を推定しないでください。TCP など HTTP 以外の脆弱性、未収集、または正確な一致がない場合は、省略するか [] を渡し、コマンド出力やログなど他の検証可能な証拠を evidence に残してください。関連付けなかった理由も説明することを推奨します。ID の推測や、通信記録を補うためだけの再調査はしないでください。"

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
