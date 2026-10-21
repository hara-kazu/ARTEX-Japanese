// Package report renders a deliverable Markdown report from the exploration
// graph: confirmed findings (with severity/evidence) (docs §15 P5).
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Input bundles what the report needs.
type Input struct {
	Title       string
	Goal        string
	GeneratedAt time.Time
	AssetCounts map[string]int
	Findings    []*db.Node
}

type findingView struct {
	VulnClass string
	Name      string
	Severity  string
	Summary   string
	PoC       string
}

func parseFinding(n *db.Node) findingView {
	var p struct {
		VulnClass string `json:"vulnclass"`
		Name      string `json:"name"`
		Severity  string `json:"severity"`
		Summary   string `json:"summary"`
		Evidence  struct {
			PoC string `json:"poc"`
		} `json:"evidence"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return findingView{p.VulnClass, p.Name, p.Severity, p.Summary, p.Evidence.PoC}
}

var sevRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "": 4}

func severityLabel(key string) string {
	labels := map[string]string{"critical": "緊急", "high": "高", "medium": "中", "low": "低", "info": "情報"}
	if label, ok := labels[key]; ok {
		return label
	}
	return key
}

func statusLabel(key string) string {
	labels := map[string]string{
		"pending": "未処理", "in_progress": "処理中", "confirmed": "確認済み", "resolved": "処理済み",
		"fixed": "修正済み", "false_positive": "誤検知", "ignored": "無視", "duplicate": "重複", "risk_accepted": "リスク受容",
	}
	if label, ok := labels[key]; ok {
		return label
	}
	return key
}

func assetTypeLabel(key string) string {
	labels := map[string]string{
		"company": "企業", "root_domain": "ルートドメイン", "subdomain": "サブドメイン", "ip": "IP",
		"service": "サービス", "app": "アプリ", "endpoint": "エンドポイント",
	}
	if label, ok := labels[key]; ok {
		return label
	}
	return key
}

// Markdown renders the report.
func Markdown(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ペネトレーションテストレポート — %s\n\n", nz(in.Title, "名前のないタスク"))
	fmt.Fprintf(&b, "- **タスクの目標**：%s\n", nz(in.Goal, "（未指定）"))
	fmt.Fprintf(&b, "- **生成日時**：%s\n\n", in.GeneratedAt.Format("2006-01-02 15:04:05"))

	// summary
	fmt.Fprintf(&b, "## 要約\n\n")
	fmt.Fprintf(&b, "- 確認済みの検出結果：**%d**件\n", len(in.Findings))
	fmt.Fprintf(&b, "- 資産：")
	var types []string
	for t := range in.AssetCounts {
		types = append(types, t)
	}
	sort.Strings(types)
	for i, t := range types {
		if i > 0 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%s %d", assetTypeLabel(t), in.AssetCounts[t])
	}
	b.WriteString("\n\n")

	// findings
	fmt.Fprintf(&b, "## 検出結果\n\n")
	if len(in.Findings) == 0 {
		b.WriteString("_今回、確認済みの脆弱性はありません。_\n\n")
	} else {
		fs := make([]findingView, 0, len(in.Findings))
		for _, n := range in.Findings {
			fs = append(fs, parseFinding(n))
		}
		sort.SliceStable(fs, func(i, j int) bool { return sevRank[fs[i].Severity] < sevRank[fs[j].Severity] })
		for i, f := range fs {
			fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, severityLabel(nz(f.Severity, "info")), nz(f.Name, nz(f.VulnClass, "未分類")))
			fmt.Fprintf(&b, "%s\n\n", nz(f.Summary, ""))
			if f.PoC != "" {
				fmt.Fprintf(&b, "**PoC / 証拠：**\n\n```\n%s\n```\n\n", f.PoC)
			}
		}
	}

	return b.String()
}

func nz(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
