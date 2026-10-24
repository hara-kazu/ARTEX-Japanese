// Package notify は脆弱性発見の IM / メールプッシュ用チャネルのアダプター層を実装する。
//
// 層構造：本パッケージは**葉のパッケージ**で、標準ライブラリーのみに依存する。データベースや server を知らない。チャネル設定は
// map[string]any として渡し（notification_channels.config という JSONB 列に対応）、
// プッシュ待ちの内容は Message として渡す。この分割により、署名計算、UTF-8 切り詰め、フィルター照合などの
// 特に誤りやすい部分を PostgreSQL なしで単体テストでき、ホストは server 側で連携を制御するだけで済む。
//
// 並行処理の約束：Channel の実装は**ステートレス**でなければならない。同じ Channel インスタンスを複数のチャネル設定
// （同じチャネルの複数のボットインスタンスも）が並行して再利用する。認証情報はすべて cfg 引数から渡し、
// webhook URL などを実装自身のフィールドにキャッシュしてはならない。
package notify

// チャネルの種類の識別子。値は notification_channels.kind の有効な集合でもあり、server 側で
// 許可リスト検証を行う（findings.status と同じく、後のチャネル追加を容易にするため DB CHECK は使わない）。
const (
	KindDingTalk = "dingtalk" // DingTalk のカスタムボット
	KindFeishu   = "feishu"   // Feishu（Lark を含む）のカスタムボット
	KindWeCom    = "wecom"    // WeCom のグループボット
	KindWebhook  = "webhook"  // 汎用 Webhook：カスタムのメソッド/ヘッダー/JSON テンプレート
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP メール
)

// イベントの種類。notification_events.kind に対応。
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind は config 内の空の kind のフォールバック値。
const InitKind = KindDingTalk

// severityRank は脆弱性のレベルを比較可能な序数に対応させる。不明なレベルは 0 を返すため、いかなる
// min_severity 設定でも不明なレベルを除外する。疑わしい場合はプッシュせず、誤検知の大量通知を避ける。
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank はレベルの序数を返す。不明なレベルは 0。
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel は emoji 付きの日本語のレベル名を返し、メッセージタイトルとカードの配色に使う。
// 不明なレベルはそのまま返し、推測して作らない。
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 緊急"
	case "high":
		return "🟠 高"
	case "medium":
		return "🟡 中"
	case "low":
		return "🔵 低"
	default:
		return severity
	}
}

// StatusLabel は対処状態を日本語に訳し、状態変更メッセージに使う。
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "未処理"
	case "in_progress":
		return "処理中"
	case "confirmed":
		return "確認済み"
	case "resolved":
		return "処理済み"
	case "fixed":
		return "修正済み"
	case "false_positive":
		return "誤検知"
	case "ignored":
		return "無視"
	case "duplicate":
		return "重複"
	case "risk_accepted":
		return "リスク受容"
	default:
		return status
	}
}

// AtLeast は severity が min の閾値以上かを判定する。min が空なら閾値なしで、すべて通す。
// 不明な severity の序数は 0 なので、空でないすべての min で拒否される（severityRank のコメントを参照）。
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
