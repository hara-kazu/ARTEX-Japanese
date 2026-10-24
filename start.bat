@echo off
rem コンソールを UTF-8 に切り替える。そうしないとこのファイルの日本語が GBK 端末で文字化けする。
chcp 65001 >nul 2>&1
rem ARTEX 監視起動スクリプト（Windows）
rem
rem 使用方法：
rem   start.bat                  フォアグラウンドで実行（Ctrl-C で停止）
rem   start.bat -addr :9000      追加引数をそのまま artex に渡す
rem
rem 担当は1つだけ：artex.exe を起動し、終了後に終了コードで再起動の要否を決める。
rem
rem   0      ユーザーによる正常停止     -> ループを終了
rem   75     プログラムが再起動を要求     -> 即座に再実行（画面で"ワンクリック更新"または"ロールバック"をクリック）
rem   その他   クラッシュ             -> バックオフ後に再実行（1->2->4…最大 60 秒）
rem
rem ダウンロード、SHA256 検証、入れ替えはここでは行わず、すべて artex 自身が起動時に実行する
rem （selfupdate パッケージ）。スクリプトは単純に保ち、詳細は start.sh 先頭の説明を参照。

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 実行ファイルが見つかりません： %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 正常に終了しました
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem 更新/ロールバックの準備完了：再実行後、artex が起動時に入れ替えを完了する。
	echo [artex] 再起動します（新しいバージョンを適用）…
	set /a delay=1
	goto loop
)

echo [artex] 異常終了 ^(code=!code!^) 。!delay!s 後に再起動 1>&2
rem timeout はリダイレクトされたコンソールで失敗するため、ping で補完する（N 秒の遅延には N+1 回必要）。
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
