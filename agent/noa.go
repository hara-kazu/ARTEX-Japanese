package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa は norma v0.4.0 で導入された「モデル駆動のコンテキスト圧縮」機構で、プラットフォームの実験機能としてユーザーが
// システム設定で切り替える。組み込み compaction とは排他:noaadapter.Enable が唯一の入口で、一度に
// コンテキスト管理器(Compactor)、Compress ツール、常駐プロンプト3区分を接続し、Enable を呼ばなければ無効
// (組み込み compaction は通常動作)。スイッチは各 agent が注入する noaEnabledFn で解決し、run ごとに
// 1回読むため、切り替えは以降に開始する run のみに影響し、agent の再構築は不要。

// enableNoa はリゾルバーが有効と報告したら noa を opts に接続する。archiveRoot は圧縮元文の永続化ベースディレクトリ
// (グローバル workDir を使い、各 agent は <workDir>/noa 配下に統一し、タスク/意図ディレクトリに分散しない)、sessionID は
// その配下のアーカイブサブディレクトリ名(グローバルに一意なので、同じベースディレクトリ内で衝突しない)。
//
// noa は実験機能:接続失敗で実タスクを中断してはならない。エラー時は onWarn で報告し、組み込み圧縮に戻す。
// 有効化成功時に opts.Compaction をクリアし、agentcore の「コンテキスト管理器が2つ同時に設定」の警告を防ぐ。
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 圧縮の有効化に失敗したため、組み込みの圧縮に戻します:" + err.Error())
		}
		return
	}
	// Compactor は Compaction に優先するが、両方あると agentcore が毎回警告するため、明示的にクリアする。
	opts.Compaction = nil
}
