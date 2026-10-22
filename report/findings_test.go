package report

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestJapaneseExportsPreserveEvidenceAndMachineCodes(t *testing.T) {
	evidence := "GET /api?q=中文\n原始レスポンス: {\"status\":\"fixed\"}"
	finding := &db.DBFinding{
		ID: 7, Name: "認可の検証", Severity: "critical", Status: "fixed", Evidence: evidence,
		TrafficBindings: []db.FindingTrafficBinding{{ID: 9, Role: "proof"}},
	}
	generated := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	markdown := FindingsMarkdown([]*db.DBFinding{finding}, generated)
	for _, want := range []string{"# 脆弱性の検出結果の集計レポート", "[CRITICAL] 認可の検証", "**状態**：fixed", "証拠 #9 · proof**", evidence} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Japanese report is missing %q:\n%s", want, markdown)
		}
	}
	single := SingleFindingMarkdown(finding, generated)
	for _, want := range []string{"[CRITICAL] 認可の検証", "**重大度**：critical", "**状態**：fixed", "証拠 #9 · proof**", evidence} {
		if !strings.Contains(single, want) {
			t.Fatalf("Single finding report is missing %q:\n%s", want, single)
		}
	}
	taskReport := Markdown(Input{
		GeneratedAt: generated,
		AssetCounts: map[string]int{"service": 2},
		Findings:    []*db.Node{{Payload: []byte(`{"severity":"high","name":"English finding"}`)}},
	})
	for _, want := range []string{"- 資産：service 2", "[HIGH] English finding"} {
		if !strings.Contains(taskReport, want) {
			t.Fatalf("Task report is missing %q:\n%s", want, taskReport)
		}
	}
	csvData := FindingsCSV([]*db.DBFinding{finding})
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(csvData, []byte{0xef, 0xbb, 0xbf}))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if rows[0][1] != "名前" || rows[0][3] != "重大度" {
		t.Fatalf("CSV headers are not Japanese: %v", rows[0])
	}
	if rows[1][0] != "7" || rows[1][3] != "critical" || rows[1][4] != "fixed" {
		t.Fatalf("CSV machine-readable values changed: %v", rows[1])
	}
}
