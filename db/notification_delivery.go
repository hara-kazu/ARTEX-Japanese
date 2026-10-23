package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// このファイルは配信タスクの取得と状態遷移を扱う。
//
// 取得には長いトランザクションでなく「リース」を使用：行を sending にして next_attempt_at を未来に進め、
// リース期限とし、コミット後にネットワーク配信を行う。これにより配信中は DB ロックを保持しない——
// ネットワークリクエストは数秒かかりうる（クライアントのタイムアウトは 15 秒）ため、行ロックを保持し続けると同じ DB の他の書き込みを圧迫する。
//
// 代償として、配信途中でプロセスがクラッシュすると行は sending に残る。ただしこれは**自己回復可能**で、リース期限後に
// next_attempt_at が過去になり、次回取得で同じ行を再取得する（取得条件の
// state IN ('pending','sending') を参照）。再試行回数は取得時点で +1 済みなので、クラッシュによって
// 無限再試行にはならず、MaxNotifyAttempts 回の機会を使い切ると failed で手動処理を待つ。

// MaxNotifyAttempts は配信1件の最大試行回数（初回を含む）。
// 配信エンジンではなくここで定義する：状態機械自体のポリシーであり、エンジンは実行役に過ぎない。
const MaxNotifyAttempts = 3

// MaxDigestBatchSize は1つの集計バッチで一度に統合する配信数の上限。
//
// 存在理由はリソース：1集計周期内で数万件の脆弱性が検出された場合（全体スキャン1回で
// 十分起こりうる）、上限がなければ取得時に全行をメモリへ読み込み、非常に長いメッセージを描画し、
// チャネルの長さ上限で大半が切り捨てられる——メモリを浪費し、切り捨てられた脆弱性を**黙って失う**。
// 上限を設けると、超過分は DB に残って次のバッチとなり、次の周期で自然に送信され、失われない。
//
// 500 の根拠：メッセージ描画後に WeCom の 4096 バイト上限内でまだ"読める内容がある"規模；
// これ以上増やしても、切り捨て位置が後ろになるだけ。
const MaxDigestBatchSize = 500

// NotificationDelivery は配信タスク1件で、描画に必要なチャネル設定とイベントスナップショットを含む。
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 結合読込する描画コンテキストで、JSON に含めない（server 層が DTO を組み立てる）。
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind はイベントから取得し、履歴一覧から脆弱性詳細への直接移動に使用する。
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind は一覧表示用の冗長フィールドで、フロントエンドの追加検索を省く。
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery は配信行の統一読込形式：配信 + イベントスナップショット + チャネル設定。
// メッセージ1件の描画には3つとも必要で、個別検索では往復が3回になる。
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery は1回の取得を表す：先に sel で候補を選んでロックし、sending に設定して
// リースを延長する。sel 内の lease 位置は呼び出し元が $n を置き、引数を自ら渡す。
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries はチャネルの期限到達済みリアルタイム配信を一括取得し、最大 limit 件とする。
//
// 「グローバルに一括取得後、選んで送る」のでなく**単一チャネル**単位で取得する：レート制限ゲートは配信エンジンでチャネルごとに
// 管理するため、そのチャネルが今回送れる件数を先に把握し、同数の行だけ取得することで、レート制限による
// 再試行回数の消費を防ぐ。逆に取得後に破棄すると、制限された行も既に attempts を1回消費し、
// 3 回の予算が待機だけで尽きて、最後は failed になる。
//
// 条件には「リース切れの sending」を含む——クラッシュからの自己回復の入口。lease は1回の
// 配信の最悪所要時間（チャネルの HTTP クライアントのタイムアウト 15 秒）より十分長い必要があり、そうでないと同じ行が2つの dispatcher で
// 同時に配信される。無効なチャネルも遮断する：無効化で既存配信は skipped となるが、
// ここでも阻止し、無効化と取得が並行した際の漏れを防ぐ。
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue はチャネルに期限到達バッチが蓄積したかを報告する：送信待ち配信があり、かつ**最古の1件**の
// 経過時間が集計周期に達していること。
//
// 判定は時刻でなく最古の配信の経過時間に基づく：新設チャネルが正時に合わせて
// すぐ1件だけの「集計」を出すことや、長く滞留したバッチがさらに1周期待つことを防ぐ。
//
// ClaimDigestBatch と分離するのは意味が異なるため：この関数は「送信すべきか」だけに答え、
// 取得ではチャネルの送信待ち行を**すべて**取る（経過時間が不足するものも含む）——そうしないと1周期が
// 複数メッセージに分かれ、集計の意味がなくなる。
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch はチャネルの現在期限到達済みの送信待ち配信を、1集計バッチとして取得する。
// 1バッチの上限は MaxDigestBatchSize 件。
//
// 同一バッチの全配信で batch_id を共有し、集合内の最小 id をバッチ番号に使用する（安定、可読、
// 追加のシーケンス不要）。再試行では COALESCE で元のバッチ番号を保持し、「この N 件は一緒に送った」という事実を
// 複数回の再試行後も維持する。
//
// ランダムでなく id 昇順の先頭 N 件を取得：最も早く生成した配信を先に送り、滞留時に
// 「新しい脆弱性が先に送られ、古いものは永久に後回し」という飢餓を防ぐ。
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit は**メモリの上限**で、呼び出し元は MaxDigestBatchSize を渡す；ここでも制限し、
	// 呼び出し元がより大きい値を渡すことを防ぐ。
	//
	// 「レート制限枠」をバッチサイズとして受け付けない：レート制限の単位はメッセージ数——1バッチは
	// メッセージ1件を送り、トークン1個を消費し、server 層の takeTokens で差し引く——「1バッチ内の
	// 脆弱性件数」とは単位が異なる。以前は rate_per_min を digest に適用するため、毎回の
	// リクエスト予算をバッチサイズとして渡し、rate=20/min のチャネルでは1バッチに脆弱性が 1 件しか入らず、
	// digest が集計文言付きリアルタイム通知に退化した。レート制限を変えるなら takeTokens の want を変更し、
	// ここは変更しないこと。
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries は「選択 + sending 設定でリース延長 + 完全な行の読込」を、すべて1トランザクション内で実行する。
// postClaim は任意の追加ステップ（集計バッチの batch_id 書き込みに使用）。
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // コミット成功後は no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending に設定し、next_attempt_at を未来へ進める：その未来時刻がリース期限となり、
	// 「リース期限前」と「再試行時刻前」を同じ条件式で表現でき、新規列が不要になる。
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent は配信群を配信済みにする。
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries は配信群を pending に戻し、再試行時刻を遅らせる。
//
// 新たな中間状態を導入せず pending に戻すことで、「残りの試行機会」を1箇所だけで
// 表現し（MaxNotifyAttempts）、再試行ポリシーによる状態機械の分岐の肥大化を防ぐ。
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries は配信群を pending に戻して即再取得可能とし、**取得時の1回分の試行を取り消す**。
//
// 用途は1つ：集計メッセージをチャネルの長さ上限で分割送信する際、このメッセージに入らなかった項目を次のバッチへ残す。
// 失敗ではないため再試行予算を消費すべきでなく——取得時に attempts は先行して +1 済みなので、
// ここで差し戻す必要がある。そうしないと 500 件の滞留を1区分 20 件の 25 区分に分けると、
// 末尾の項目が第 3 区分で MaxNotifyAttempts により failed と判定され、一度もエラーがなくても失敗する。
//
// GREATEST(...,0) は「手動再送で attempts がゼロにされた後、再びここに到達した」場合を保護し、
// カウントが負にならないようにする。
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries は配信群を最終失敗にし、配信履歴からの手動再送を待つ。
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// プレースホルダーは $3 から：$1 は state、$2 は last_error。
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery は配信1件を手動再送：pending に戻し、再試行数をゼロにし、
// 直ちに期限到達とする。回数の初期化は意図的——人が「再送」を押すのは過去の失敗原因を処理済みという意味なので、
// 旧カウントで制限する理由はない。
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("配信%dが存在しないか、現在の状態では再送できません", id)
	}
	return nil
}

// NotificationDeliveryFilter は配信履歴の検索条件。
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries は配信履歴をページ単位で新しい順に返す。
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError はエラー情報を列が受け付ける長さに切り詰める。チャネルのレスポンス本文は長い場合があり、
// （汎用 Webhook で自前サービスを呼ぶ場合は特に）、切り詰めないと履歴一覧のペイロードが肥大化する。
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 文字境界まで戻り、UTF-8 文字が途中で切れてフロントエンドで文字化けすることを防ぐ。
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders は start から始まる $n のプレースホルダー文字列と対応引数を生成し、IN (...) で使用する。
// 例えば start=3, ids=[7,8] → "$3,$4", [7,8]。
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
