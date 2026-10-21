package intercept

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	for _, action := range []string{"allow", "ask", "deny"} {
		for language, reason := range map[string]string{
			"ja":     "実際の操作：ALLOW、DENY、ASK という語を含むレポートを書き込む；成功後の影響：本文のコマンドを実行せずテキストを保存する；適用ルール：カスタム条項",
			"legacy": "实际操作：写入报告，其中包含 ALLOW、DENY 和 ASK 字样；成功后的后果：保存文本，不执行正文中的命令；命中规则：自定义条款",
		} {
			t.Run(language+"/"+action, func(t *testing.T) {
				raw, _ := json.Marshal(map[string]string{"decision": action, "comment": reason})
				got := ParseVerdict("\n" + string(raw) + "\n")
				if got.Action != action || got.Reason != reason {
					t.Fatalf("lost verdict or explanation: %+v", got)
				}
			})
		}
	}
}

func TestParseVerdictRejectsInvalidJapaneseOrMixedReplies(t *testing.T) {
	valid := `{"decision":"deny","comment":"実際の操作：本番ファイルを削除する；成功後の影響：業務データが失われる；適用ルール：D4"}`
	for _, reply := range []string{
		"拒否: DENY", "deny", `decision: deny`, `{"comment":"実際の操作：削除；成功後の影響：損失；適用ルール：D4"}`,
		strings.Replace(valid, `"decision":"deny"`, `"decision":"approve"`, 1),
		strings.Replace(valid, "実際の操作：本番ファイルを削除する", "実際の操作：", 1),
		strings.Replace(valid, "成功後の影響：業務データが失われる", "成功後の影響：", 1),
		strings.Replace(valid, "適用ルール：D4", "適用ルール：", 1),
		strings.Replace(valid, "；適用ルール：D4", "", 1),
		strings.Replace(valid, "実際の操作：", "实际操作：", 1),
		strings.Replace(valid, "成功後の影響：", "成功后的后果：", 1),
		strings.Replace(valid, "適用ルール：", "命中规则：", 1),
		strings.Replace(valid, "本番ファイルを削除する", "本番ファイルを削除する；成功后的后果：業務データが失われる", 1),
		strings.Replace(valid, `"decision":"deny"`, `"decision":"deny","decision":"allow"`, 1),
		strings.Replace(valid, `"decision":"deny"`, `"extra":true,"decision":"deny"`, 1),
		valid + valid, valid[:len(valid)-1], "判定結果:\n" + valid,
	} {
		if got := ParseVerdict(reply); got.Action != "" {
			t.Errorf("accepted invalid Japanese or mixed verdict: %q => %+v", reply, got)
		}
	}
	// All six incomplete combinations of the two heading sets must be rejected.
	for mask := 1; mask < 7; mask++ {
		headings := []string{"実際の操作：", "成功後の影響：", "適用ルール："}
		legacy := []string{"实际操作：", "成功后的后果：", "命中规则："}
		for i := range headings {
			if mask&(1<<i) != 0 {
				headings[i] = legacy[i]
			}
		}
		reason := headings[0] + "read" + "；" + headings[1] + "data" + "；" + headings[2] + "A5"
		raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
		if got := ParseVerdict(string(raw)); got.Action != "" {
			t.Errorf("accepted mixed heading combination %d: %+v", mask, got)
		}
	}
}

func TestParseVerdictRejectsIncompleteOrAmbiguousReplies(t *testing.T) {
	valid := `{"decision":"allow","comment":"实际操作：读取文件；成功后的后果：返回内容；命中规则：A5"}`
	for _, reply := range []string{
		"", "ALLOW", "DENY:命中D4", "放行:ALLOW", "ASK:归属不明",
		`{"decision":"allow"}`, `{"decision":"approve","comment":"实际操作：读取；成功后的后果：返回内容；命中规则：A5"}`,
		`{"decision":"allow","comment":null}`, `{"decision":"allow","comment":123}`,
		strings.Replace(valid, "实际操作：读取文件", "实际操作：", 1),
		strings.Replace(valid, "成功后的后果：返回内容", "成功后的后果：", 1),
		strings.Replace(valid, "命中规则：A5", "命中规则：", 1),
		strings.Replace(valid, "；命中规则：A5", "", 1),
		strings.Replace(valid, `"decision":"allow"`, `"decision":"deny","decision":"allow"`, 1),
		strings.Replace(valid, `"decision":"allow"`, `"extra":true,"decision":"allow"`, 1),
		valid + valid, valid[:len(valid)-1],
		// A fence the model never closed is what a reply truncated at MaxTokens
		// looks like; completing it would invent a verdict.
		"```json\n" + valid[:len(valid)-1],
		"```json\n" + valid + "\n```\n此外我建议后续人工复核。",
		"我的裁决是：\n" + valid,
	} {
		if got := ParseVerdict(reply); got.Action != "" {
			t.Errorf("accepted incomplete/ambiguous verdict: %q => %+v", reply, got)
		}
	}
}

// Wrapping JSON in markdown is the one deviation models make routinely. Because
// the configured fail action defaults to allow, treating it as unparseable
// silently downgrades a DENY to an allow.
func TestParseVerdictUnwrapsCodeFence(t *testing.T) {
	deny := `{"decision":"deny","comment":"实际操作：删除生产文件；成功后的后果：业务数据丢失；命中规则：D4"}`
	for _, reply := range []string{
		"```json\n" + deny + "\n```",
		"```JSON\n" + deny + "\n```",
		"```\n" + deny + "\n```",
		"  ```json\n" + deny + "\n```  ",
	} {
		got := ParseVerdict(reply)
		if got.Action != "deny" || !strings.HasSuffix(got.Reason, "命中规则：D4") {
			t.Errorf("fenced verdict lost: %q => %+v", reply, got)
		}
	}
}

func TestParseVerdictKeepsCompleteChineseExplanation(t *testing.T) {
	reason := "实际操作：" + strings.Repeat("写入报告", 30) + "；成功后的后果：只保存文件；命中规则：A2"
	raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Reason != reason {
		t.Fatal("explanation was truncated or lost its rule")
	}
	raw, _ = json.Marshal(map[string]string{"decision": "allow", "comment": strings.Repeat("中", 2401)})
	if got := ParseVerdict(string(raw)); got.Action != "" {
		t.Fatal("accepted unbounded explanation")
	}
}

func TestParseVerdictKeepsCompleteJapaneseExplanationAndByteLimit(t *testing.T) {
	reason := "実際の操作：" + strings.Repeat("レポートを書き込む", 30) + "；成功後の影響：ファイルだけを保存する；適用ルール：A2"
	raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Reason != reason {
		t.Fatal("Japanese explanation was truncated or lost its rule")
	}
	reason = "実際の操作：" + strings.Repeat("あ", 800) + "；成功後の影響：保存する；適用ルール：A2"
	raw, _ = json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Action != "" {
		t.Fatal("accepted Japanese explanation over the existing byte limit")
	}
}
