package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "モデルは今回のターンを正常終了しましたが、要約文を残していません。事実と資産は今回のツール呼び出し記録を参照してください",
	harness.ReasonMaxTurns:          "ステップ数の上限(MaxTurns)に達しました。SDK は終了処理を実行し、事実と資産を書き戻しました。インテントは失敗として扱わず exhausted とし、計画エージェントが方向を変えて続行できるようにします",
	harness.ReasonTimeout:           "単一実行の実時間の予算(MaxDuration)に達しました。期限に達すると実行中のツールを中断してその場で終了処理に入り、特定済みの事実と資産を書き戻して、インテントを exhausted にします",
	harness.ReasonModelError:        "モデルまたは API の呼び出しに失敗しました（ネットワーク、認証、レート制限、プロバイダーの 5xx など）。再試行を使い切るとインテントは blocked になります。転送層の障害によって、このインテントの探索はほとんど実行できていません。実行過程（get_worker_trace）を確認してから、再割り当てするか手段を変えるか判断してください",
	harness.ReasonBlockingLimit:     "コンテキスト長のハード上限に達したため、リクエストは送信前に止められました。インテントの範囲を狭めるか、ツールの返り値を圧縮してください",
	harness.ReasonPromptTooLong:     "プロンプトが長すぎ、コンテキスト圧縮の再試行も使い切ったため、続行できません",
	harness.ReasonImageError:        "現在のモデルは今回のマルチモーダル内容に対応していません。画像に対応したモデルに切り替えるか、ツールが画像を返さないようにしてください",
	harness.ReasonStopHookPrevented: "Stop フックが今回のターンの終了を止め、その後続行できませんでした。タスクの Guard ルールが厳しすぎないか確認してください",
	harness.ReasonHookStopped:       "範囲外の対象や禁止コマンドなどを理由に、ツールまたはフックが明示的に実行を停止しました。最後の tool_result のブロック理由を確認してください",
	harness.ReasonAbortedStreaming:  "モデルの出力のストリーミング生成中に実行が取り消されました",
	harness.ReasonAbortedTools:      "ツールの実行中に処理が取り消されました",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "取り消し理由を取得できませんでした"
		}
		stage := "実行中"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "モデルの出力段階"
		case harness.ReasonAbortedTools:
			stage = "ツールの実行段階"
		}
		sum = "（実行が中断されました：" + short + "；停止した段階：" + stage + progressSuffix(term, tr) + "、未完了）"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "（実行予算の上限に達しました(" + string(reason) + ")。終了処理で事実を書き戻しました" + progressSuffix(term, tr) + "；今回は要約文がありません）"
	} else {
		hint := terminalReasonHint(reason)
		sum = "（要約文なし、最終状態 " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **最終状態**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **中断理由** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **中断理由**: 取得できません。取り消し側が context.WithCancelCause で名前付きの理由を付けていない可能性があります\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **下位層のエラー**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **取り消し前に生成された部分的な出力**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **実行済み**: モデルのターン%d回\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **今回の実行時間**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **累計 token**: 入力 %d / 出力 %d / キャッシュ読み取り %d / キャッシュ書き込み %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **ツール呼び出し**: 今回の実行はツールを呼び出す前に終了しました\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **中断時に実行中だったツール**: `%s`（実行時間 %s、**結果未応答**）\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **中断前の最後のツール**: `%s`（正常に応答済み）\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "実行の context は取り消されましたが、下位層で Terminal イベントが生成されていません"
	}
	return "不明な最終状態です。harness に TerminalReason が追加された可能性があります。reasonHint を補ってください"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d回", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "、実行済み " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
