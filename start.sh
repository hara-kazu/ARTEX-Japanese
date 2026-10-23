#!/bin/sh
# ARTEX 監視起動スクリプト（Linux / macOS / Docker ENTRYPOINT）
#
# 使用方法：
#   ./start.sh                       フォアグラウンド実行（Ctrl-C で停止）
#   nohup ./start.sh >artex.log 2>&1 &   バックグラウンドで常駐
#   ./start.sh -addr :9000           追加引数をそのまま artex へ渡す
#
# 担当は1つだけ：artex を起動し、終了後に終了コードで再起動の要否を決める。
#
#   0      ユーザーによる正常停止        → ループ終了
#   75     プログラムが再起動を要求        → 即座に再実行（画面で"ワンクリック更新"または"ロールバック"をクリック）
#   その他   クラッシュ                → バックオフ後に再実行（1→2→4…最大 60 秒）
#
# ダウンロード、SHA256 検証、入れ替えは意図的にここで行わない：sh と bat でそれらのロジックを二重に書く必要があり、
# 最も失敗できない部分でもある——起動できないバイナリへ入れ替えると、このスクリプトは忠実に
# 再起動を繰り返し、ユーザーがマシン上で手動復旧するしかない。検証/入れ替えはすべて Go（selfupdate パッケージ）に残し、
# artex 自身が起動時に完了させ、スクリプトを単純に保つ。
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 実行ファイルが見つかりません： $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 停止シグナルを artex 本体へ転送する。
#
# Docker では必須：docker stop は SIGTERM を PID 1（このスクリプト）だけへ送り、
# 子プロセスへは送らない。転送しないと artex が受け取れず正常な終了処理ができないため、10 秒後に SIGKILL で
# 強制終了され、実行中のタスクが途中で切れる。
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# シグナルは wait を中断して >128 を返させる。この時点では子プロセスはまだ正常な終了処理中で、
	# 本当の終了コードを得るためにもう一度 wait が必要。
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 停止しました"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 正常に終了しました"
			exit 0
			;;
		"$RESTART_CODE")
			# 更新/ロールバックの準備完了：再実行後、artex が起動時に入れ替えを完了する（selfupdate.Bootstrap を参照）。
			echo "[artex] 再起動します（新しいバージョンを適用）…"
			delay=1
			;;
		*)
			echo "[artex] 異常終了 (code=$code)。${delay}s 後に再起動" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
