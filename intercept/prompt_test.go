package intercept

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	for _, action := range []string{"allow", "ask", "deny"} {
		t.Run(action, func(t *testing.T) {
			reason := "実際の操作：写入报告，其中包含 ALLOW、DENY 和 ASK 字样；成功後の影響：保存文本，不执行正文中的命令；適用ルール：自定义条款"
			raw, _ := json.Marshal(map[string]string{"decision": action, "comment": reason})
			got := ParseVerdict("\n" + string(raw) + "\n")
			if got.Action != action || got.Reason != reason {
				t.Fatalf("lost verdict or explanation: %+v", got)
			}
		})
	}
}

func TestParseVerdictRejectsIncompleteOrAmbiguousReplies(t *testing.T) {
	valid := `{"decision":"allow","comment":"実際の操作：读取文件；成功後の影響：返回内容；適用ルール：A5"}`
	for _, reply := range []string{
		"", "ALLOW", "DENY:命中D4", "放行:ALLOW", "ASK:归属不明",
		`{"decision":"allow"}`, `{"decision":"approve","comment":"実際の操作：读取；成功後の影響：返回内容；適用ルール：A5"}`,
		`{"decision":"allow","comment":null}`, `{"decision":"allow","comment":123}`,
		strings.Replace(valid, "実際の操作：读取文件", "実際の操作：", 1),
		strings.Replace(valid, "成功後の影響：返回内容", "成功後の影響：", 1),
		strings.Replace(valid, "適用ルール：A5", "適用ルール：", 1),
		strings.Replace(valid, "；適用ルール：A5", "", 1),
		strings.Replace(valid, `"decision":"allow"`, `"decision":"deny","decision":"allow"`, 1),
		strings.Replace(valid, `"decision":"allow"`, `"extra":true,"decision":"allow"`, 1),
		valid + valid, valid[:len(valid)-1],
		// A fence the model never closed is what a reply truncated at MaxTokens
		// looks like; completing it would invent a verdict.
		"```json\n" + valid[:len(valid)-1],
		"```json\n" + valid + "\n```\n此外我建议后续人工复核。",
		"我的裁决是：\n" + valid,
	} {
		legacy := strings.NewReplacer("実際の操作：", "实际操作：", "；成功後の影響：", "；成功后的后果：", "；適用ルール：", "；命中规则：").Replace(reply)
		for _, localized := range []string{reply, legacy} {
			if got := ParseVerdict(localized); got.Action != "" {
				t.Errorf("accepted incomplete/ambiguous verdict: %q => %+v", localized, got)
			}
		}
	}
}

func TestParseVerdictLegacyChineseContract(t *testing.T) {
	reason := "实际操作：读取文件；成功后的后果：返回内容；命中规则：A5"
	for _, action := range []string{"allow", "ask", "deny"} {
		raw, _ := json.Marshal(map[string]string{"decision": action, "comment": reason})
		for _, reply := range []string{string(raw), "```json\n" + string(raw) + "\n```"} {
			if got := ParseVerdict(reply); got.Action != action || got.Reason != reason {
				t.Fatalf("lost legacy verdict: %q => %+v", reply, got)
			}
		}
	}
	for _, mixed := range []string{
		strings.Replace(reason, "实际操作：", "実際の操作：", 1),
		strings.Replace(reason, "；成功后的后果：", "；成功後の影響：", 1),
		strings.Replace(reason, "；命中规则：", "；適用ルール：", 1),
	} {
		raw, _ := json.Marshal(map[string]string{"decision": "deny", "comment": mixed})
		if got := ParseVerdict(string(raw)); got.Action != "" {
			t.Fatalf("accepted mismatched explanation fields: %+v", got)
		}
	}
}

// Wrapping JSON in markdown is the one deviation models make routinely. Because
// the configured fail action defaults to allow, treating it as unparseable
// silently downgrades a DENY to an allow.
func TestParseVerdictUnwrapsCodeFence(t *testing.T) {
	deny := `{"decision":"deny","comment":"実際の操作：删除生产文件；成功後の影響：业务数据丢失；適用ルール：D4"}`
	for _, reply := range []string{
		"```json\n" + deny + "\n```",
		"```JSON\n" + deny + "\n```",
		"```\n" + deny + "\n```",
		"  ```json\n" + deny + "\n```  ",
	} {
		got := ParseVerdict(reply)
		if got.Action != "deny" || !strings.HasSuffix(got.Reason, "適用ルール：D4") {
			t.Errorf("fenced verdict lost: %q => %+v", reply, got)
		}
	}
}

func TestParseVerdictKeepsCompleteChineseExplanation(t *testing.T) {
	reason := "実際の操作：" + strings.Repeat("写入报告", 30) + "；成功後の影響：只保存文件；適用ルール：A2"
	raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Reason != reason {
		t.Fatal("explanation was truncated or lost its rule")
	}
	raw, _ = json.Marshal(map[string]string{"decision": "allow", "comment": strings.Repeat("中", 2401)})
	if got := ParseVerdict(string(raw)); got.Action != "" {
		t.Fatal("accepted unbounded explanation")
	}
}
