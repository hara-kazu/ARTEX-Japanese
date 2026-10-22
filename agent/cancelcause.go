package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "ユーザーがタスクを一時停止しました",
		"ユーザーがタスク制御インターフェース（POST /api/tasks/{id}/control、action=pause）でタスクを一時停止しました。今回の Planner/Worker の実行は明示的に取り消されました。実行中のインテントは frontier(open) に戻り、タスクの再開後に再取得して最初から実行します")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "オーケストレーション Agent がタスクを一時停止しました",
		"オーケストレーション Agent が pause_task ツールでこのタスクを一時停止しました。今回の Planner/Worker の実行は明示的に取り消されました。実行中のインテントは frontier(open) に戻り、再開後に再実行します")
	AbortTaskDeleted = cause("task_deleted", "タスクが削除されました",
		"タスクを削除中です（DELETE /api/tasks/{id}）。削除バリアによって、このタスクで実行中の Planner、Worker、メイン Agent は取り消されました。今回の実行結果は使用されません")
	AbortPausedOnReload = cause("paused_on_reload", "バックエンドがタスクの一時停止状態を復元しました",
		"バックエンドの起動時に、データベースに保存された状態に基づいてタスクの一時停止状態を復元しました。今回の実行は取り消されました。通常、復元段階で実行中の Agent はありません")
	AbortGoalMet = cause("goal_met", "計画エージェントがタスクの目標を達成済みと判定しました",
		"計画エージェントがタスクの目標を達成済みと判定して done にし、その後、まだ実行中の Worker を取り消しました。これらのインテントは失敗ではなく stopped になります")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "タスクのタイムアウト時の終了処理の待機時間を使い切りました",
		"タスクが timeout に達してから、実行中の Worker の正常な終了処理を待機しましたが、90秒の drain 猶予でも完了しなかったため、強制的に取り消しました。インテントは exhausted になり、終了処理ですでに保存した事実と資産は保持します")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "計画エージェントがこのインテントを終了しました",
		"計画エージェントが kill_work を呼び出して、このインテントを明示的に終了しました。通常は方向がずれたか、継続する価値がなくなったことを表します。インテントは stopped になり、自動で再取得しません")
	AbortWorkPausedByUser = cause("work_paused_by_user", "ユーザーがこの Worker のインテントを一時停止しました",
		"ユーザーが実行中の Worker を一時停止しました。今回の呼び出しは取り消され、インテントは paused になります。登録済みのインテント、事実、脆弱性、活動記録はすべて保持し、再開後は最初から実行します")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "ユーザーがこの Worker のインテントを削除しました",
		"ユーザーが実行中の Worker を削除しました。今回の呼び出しは取り消されました。Worker が書き込み区間を抜けた後、サーバーはユーザーが選んだ削除方式でインテントを処理します。論理削除では削除済みと記録するだけで成果をすべて保持し、物理削除ではこのインテントと、それだけを根拠とする下流ノードを連鎖的に削除します")
	AbortWorkFinished = cause("work_finished", "Worker が正常終了し、context を解放しました",
		"Worker が正常終了し、エンジンが detachWork で context のリソースを解放しました。実行の中断ではありません。中断メッセージに表示された場合は、取り消しと終了イベントの競合が発生したことを表します")
	AbortPausedRaceGuard = cause("paused_race_guard", "タスクの一時停止中は新しい実行の開始を拒否します",
		"タスクの一時停止中は、claim と一時停止の競合によって Worker が起動し続けるのを防ぐため、エンジンは新しい実行 context の発行を拒否します。取得済みのインテントは frontier に戻ります")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "ユーザーが今回のチャットのターンを停止しました",
		"ユーザーが停止をクリックし、今回のメイン Agent またはチャット Agent の実行を明示的に中止しました。生成済みの活動記録は保持し、次のメッセージを送信して続行できます")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "タスクを一時停止し、メイン Agent のチャットを中止しました",
		"ユーザーがタスクを一時停止した際、実行中のメイン Agent のチャットも同時に取り消されました。生成済みの活動記録は保持し、タスクの再開後も今回のメッセージを自動で再実行しません")
	AbortChatTurnFinished = cause("chat_turn_finished", "今回のチャットのターンが正常終了し、context を解放しました",
		"今回のチャットのターンが正常終了し、サーバーがその context のリソースを解放しています。実行の中断ではありません。中断メッセージに表示された場合は、取り消しと終了イベントの競合が発生したことを表します")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "バックエンドプロセスを終了しています",
		"バックエンドプロセスが SIGINT または SIGTERM を受け取り、再起動、更新、または終了しています。実行中の Agent はすべて取り消されます。再起動後、残っている running のインテントは open に戻して再実行します")
	AbortRunHardTimeout = cause("run_hard_timeout", "単一実行の最終的な強制タイムアウトが発生しました",
		"単一の実行が実時間のソフト予算と追加の猶予を超えました。モデルのリクエストかツールが長時間応答せず、通常のターン境界で終了処理を実行できなかったことを表します。中断前の最後の未応答のツール呼び出しを重点的に確認してください")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "上位の context が deadline に達しました",
			"上位の context が deadline に達しましたが、設定側が WithTimeoutCause で名前付きの理由を付けていません: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "取り消し側が名前付きの理由を付けていません",
			"上位の context が取り消されましたが、取り消し側が context.WithCancelCause で名前付きの理由を付けていません。agent/cancelcause.go に理由を登録し、該当する取り消し箇所に接続してください", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
