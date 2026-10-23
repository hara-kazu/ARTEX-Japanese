package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 不正な JSON、空の入力、型が違うフィールドは、すべてゼロ値の Filter に退化する必要がある。
	// つまり「フィルターなし」。この不変条件は「漏らすより多めにプッシュ」という方針を具体化する：
	// ここをエラーや部分的な解析に変えると、ユーザーが1文字誤設定するだけで高リスクの通知がすべて黙って失われる。
	cases := []struct {
		name string
		raw  string
	}{
		{"空の入力", ""},
		{"無効な JSON", `{not json`},
		{"途中で切れた JSON", `{"min_severity":`},
		{"型の不一致", `{"min_severity": 123, "task_ids": "abc"}`},
		{"最上位が配列", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("不正な設定はゼロ値の Filter にフォールバックするべきです。実際の値: %+v", f)
			}
			// ゼロ値の Filter は任意のイベントに一致する必要がある。
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("ゼロ値の Filter はすべてのイベントに一致するべきです")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 不明なレベルの序数は 0 で、空でないすべての閾値で阻止すること（疑わしい場合はプッシュしない）。
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 期待値 %v、実際の値 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL注入",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"空の範囲は制限なし", Filter{}, true},
		{"タスクが一致", Filter{TaskIDs: []int64{7}}, true},
		{"タスクが一致しない", Filter{TaskIDs: []int64{8}}, false},
		{"複数のタスクのいずれかが一致", Filter{TaskIDs: []int64{8, 7}}, true},
		{"資産に共通部分がある", Filter{AssetIDs: []int64{20, 99}}, true},
		{"資産に共通部分がない", Filter{AssetIDs: []int64{99}}, false},
		{"タスクと資産の両方が一致", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"タスクは一致するが資産は一致しない", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("期待値 %v、実際の値 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include が空なら全件受信", Filter{}, "任意类型", true},
		{"include に一致", Filter{VulnClassInclude: []string{"SQL"}}, "SQL注入", true},
		{"include に一致しない", Filter{VulnClassInclude: []string{"命令执行"}}, "SQL注入", false},
		{"include の複数語のいずれかが一致", Filter{VulnClassInclude: []string{"命令执行", "SQL"}}, "SQL注入", true},
		{"大文字と小文字を区別しない", Filter{VulnClassInclude: []string{"sql"}}, "SQL注入", true},
		{"exclude に一致したら即座に除外", Filter{VulnClassExclude: []string{"信息泄露"}}, "信息泄露", false},
		{"exclude に一致しなければ許可", Filter{VulnClassExclude: []string{"信息泄露"}}, "SQL注入", true},
		// 除外は包含に優先する：両方に一致する場合は除外すること。
		{"除外を包含より優先", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"注入"},
		}, "SQL注入", false},
		// 空白のみのキーワードは無視すること。しないと「空白を含むすべての文字列に一致」に退化する。
		{"空白のキーワードを無視", Filter{VulnClassInclude: []string{"", "  "}}, "SQL注入", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("期待値 %v、実際の値 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// デフォルトは無効：大半の人が言う「脆弱性のプッシュ」とは新しい脆弱性の発見であり、状態の逐次記録ではない。
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("状態変更イベントは無効時にはスキップされるべきです")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change を有効にすると状態変更イベントが一致するべきです")
	}
	// 作成イベントは on_status_change の影響を受けない。
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("作成イベントは on_status_change に依存するべきではありません")
	}
}
