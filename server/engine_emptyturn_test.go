package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 空回りターン（思考のみで本文もツールもない）の検出と続行。steerHooks.Stop を参照。

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "先扫端口"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"思考のみ", []llm.Message{llm.UserText("开始"), assistantThinking("想想")}, true},
		{"思考+ツール", []llm.Message{llm.UserText("开始"), toolUse}, false},
		{"思考+本文", []llm.Message{assistantThinking("想想"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("结论")},
		}}, false},
		{"本文が空白文字のみ", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"完全に空の assistant ターン", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// ツール結果は user の役割なので、その直前の assistant まで戻って判定し、近いメッセージを誤認しないこと。
		{"最後のメッセージがツールの結果", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant のメッセージがない", []llm.Message{llm.UserText("开始")}, false},
		{"空の履歴", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks はプログラム可能な inner HookRunner で、steerHooks が inner の決定を尊重することを検証します。
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("我应该先枚举子域名")}

	t.Run("空回りのターンに継続指示を挿入", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("空回りのターンを強制停止すべきではありません")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("本文またはツールがあれば介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("已完成扫描，未发现开放端口")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("正常な終了を空回りと誤判定しました: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("介入していない場合はカウントすべきではありません, got %d", n)
		}
	})

	t.Run("上限に達したら終了処理を許可", func(t *testing.T) {
		const limit = 5 // ユーザーが「空応答の再試行回数」を 5 に設定
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d 回目はまだ割り当て内であるべきです, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("上限を超えても挿入しています: %v", blocking)
		}
	})

	// 「空応答の再試行回数」を -1 にするとこの層を無効化し、emptyTurnNudgeLimit は 0 に解決します。
	t.Run("設定が無効なら介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("無効にしても挿入しています: %v", blocking)
		}
	})

	t.Run("inner が強制停止を決定したら追加しない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard 拒绝收场"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard 拒绝收场" || blocking != nil {
			t.Fatalf("inner の強制停止が書き換えられています: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner に処理を譲る場合は割り当てを消費すべきではありません, got %d", n)
		}
	})

	t.Run("inner が継続を要求済みなら追加しない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard 的续跑理由"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard 的续跑理由" {
			t.Fatalf("inner の継続メッセージが書き換えられています: %v", blocking)
		}
	})

	t.Run("カウンターがない場合は動作を変えない", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 将来、別の呼び出し元が nudges を渡し忘れた場合など
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("カウンターがない場合は挿入すべきではありません: %v", blocking)
		}
	})
}
