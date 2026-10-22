package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestTranslationCompatMentionLookup(t *testing.T) {
	s, findingID := newRetestServer(t)
	for _, label := range []string{"漏洞", "脆弱性"} {
		message := fmt.Sprintf("@[%s#%d example]", label, findingID)
		expanded, err := composeChatMentionMessage(s.m.pg, message)
		if err != nil || !strings.HasPrefix(expanded, message) || !strings.Contains(expanded, "original proof") || !strings.Contains(expanded, fmt.Sprintf("脆弱性 #%d:", findingID)) {
			t.Fatalf("reference not resolved: %q %v", expanded, err)
		}
	}
	if _, err := composeChatMentionMessage(s.m.pg, "@[漏洞#9223372036854775807 missing]"); err == nil {
		t.Fatal("missing legacy reference bypassed validation")
	}
}

func TestTranslationCompatLogLevels(t *testing.T) {
	for want, words := range map[string][]string{
		"error": {"fatal", "panic", "ERROR", "err:", "失敗", "破棄", "拒否", "✕", "到達不能", "失败", "丢弃", "拒绝", "不可达"},
		"warn":  {"WARN", "disabled", "無効", "skip", "stopped", "⚠", "再試行", "禁用", "重试"},
		"info":  {"done", "正常に完了", "任务完成"},
	} {
		for _, word := range words {
			if got := levelOf("[tool] " + word); got != want {
				t.Errorf("levelOf(%q)=%s want=%s", word, got, want)
			}
		}
	}
	for _, mixed := range []string{"warn error", "禁用 失敗", "再試行 连接失败"} {
		if got := levelOf(mixed); got != "error" {
			t.Errorf("error priority lost for %q: %s", mixed, got)
		}
	}
}

func TestTranslationCompatFallbackCommands(t *testing.T) {
	s, _ := newRetestServer(t)
	for _, tc := range []struct{ input, kind, want string }{
		{"意图 payload", db.KindIntent, "payload"},
		{"インテント payload", db.KindIntent, "payload"},
		{"intent payload", db.KindIntent, "payload"},
		{"提示 payload", db.KindHint, "payload"},
		{"ヒント payload", db.KindHint, "payload"},
		{"hint payload", db.KindHint, "payload"},
		{"意图intent payload", db.KindIntent, "payload"},
		{"提示hint payload", db.KindHint, "payload"},
		{"INTENT payload", db.KindIntent, "INTENT payload"},
		{"HINT payload", db.KindHint, "HINT payload"},
		{"意图インテント payload", db.KindIntent, "インテント payload"},
		{"インテント意图 payload", db.KindIntent, "意图 payload"},
		{"提示ヒント payload", db.KindHint, "ヒント payload"},
		{"ヒント提示 payload", db.KindHint, "提示 payload"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			created, err := s.m.pg.CreateTask("fallback compatibility", "goal", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.m.pg.DeleteTask(created.ID) })
			task := &Task{ID: strconv.FormatInt(created.ID, 10), Store: s.m.pg.Exploration(created.ExplorationID)}
			reply := s.fallbackChat(task, tc.input)
			nodes, err := task.Store.ListByKind(tc.kind, 10)
			if err != nil || len(nodes) != 1 {
				t.Fatalf("nodes=%+v err=%v", nodes, err)
			}
			field, state, priority, prefix := "text", "active", 0, "ヒントを記録しました"
			if tc.kind == db.KindIntent {
				field, state, priority, prefix = "summary", "open", 9, "優先度の高いインテントを1件追加しました"
			}
			var payload map[string]any
			if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload[field] != tc.want || nodes[0].Kind != tc.kind || nodes[0].State != state || nodes[0].Priority != priority || nodes[0].Origin != "human" {
				t.Fatalf("unexpected saved command: node=%+v payload=%v", nodes[0], payload)
			}
			if !strings.HasPrefix(reply, prefix) || !strings.HasSuffix(reply, tc.want) {
				t.Fatalf("unexpected Japanese reply: %q", reply)
			}
		})
	}
}

func TestTranslationCompatConversationTitles(t *testing.T) {
	s, _ := newRetestServer(t)
	setRetestProvider(s, retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("確認しました")}}, "end_turn", llm.Usage{}, nil
	}})
	for _, title := range []string{"新对话", "新しいチャット", "", "custom title", "新对话-custom"} {
		t.Run(title, func(t *testing.T) {
			conversation, err := s.m.pg.CreateConversation("auto", title, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { waitRetestIdle(t, s); _ = s.m.pg.DeleteConversation(conversation.ID) })
			w := retestRequest(s.pgSendConversationMessage, http.MethodPost, conversation.ID, `{"message":"first message"}`)
			if w.Code != http.StatusAccepted {
				t.Fatalf("send status=%d body=%s", w.Code, w.Body.String())
			}
			waitRetestIdle(t, s)
			stored, err := s.m.pg.GetConversation(conversation.ID)
			if err != nil || stored == nil {
				t.Fatalf("conversation=%+v err=%v", stored, err)
			}
			want := title
			if title == "" || title == "新对话" || title == "新しいチャット" {
				want = "first message"
			}
			if stored.Title != want {
				t.Fatalf("title=%q want=%q", stored.Title, want)
			}
		})
	}
}
