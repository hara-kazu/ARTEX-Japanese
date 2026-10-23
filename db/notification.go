package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// このファイルは IM 通知のチャネル設定とイベント層。配信タスクの取得と状態遷移は
// db/notification_delivery.go。
//
// このファイルを変更する際は、次の2つの不変条件を必ず維持すること：
//
//  1. 脆弱性を書き込むトランザクション(RecordFindingTx)は InsertNotificationEventTx のブラインド挿入を1回だけ呼び、
//     通知関連テーブルを読まず、フィルター照合もしない。ここに読み取り処理を加えると、
//     ユーザーの誤ったフィルター設定によって脆弱性の書き込みトランザクションを汚染、あるいは中止させる可能性がある。
//  2. フィルター照合はエラーにしない：不正な設定は一律「一致」と扱う(notify.Match を参照)。余分な通知は許容しても、
//     通知漏れは許容しない。

// ErrNotificationChannelNotFound はチャネルが存在しないことを表す。
var ErrNotificationChannelNotFound = errors.New("通知チャンネルが存在しません")

// 配信状態。
const (
	NotifyStatePending = "pending" // 送信待ち
	NotifyStateSending = "sending" // dispatcher が取得済みで、リース期限前
	NotifyStateSent    = "sent"    // 配信済み
	NotifyStateFailed  = "failed"  // 再試行上限または恒久的失敗で、手動再送可能
	NotifyStateSkipped = "skipped" // チャネルが停止され、送信しない
)

// 通知モード。
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode は通知モードを許可リストで検証する（findings.status と同様に DB CHECK は使わず、
// 今後の拡張を容易にする）。
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel はチャネルインスタンスの設定。Config と Filter は元の JSON を保持し、
// 解析は notify パッケージに任せ、db 層ではフィールドの意味を扱わない。
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled をポインターにして「フィールド未送信」と「明示的な false」を区別する——
	// フロントエンドのスイッチは変更したフィールドのみ送信する。
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled はチャネルが有効かを返す；Enabled が nil（未読込）なら有効と扱う。
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent はイベント事実1件。
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels は全チャネルインスタンスを返し、有効なものを先頭に、同順位は id 順にする。
// ソートを SQL で行い、UI と dispatcher が同じ安定した順序を見るようにする。
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID はチャネル1件を取得する。
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel はチャネルを新規作成または更新する。
//
// 更新時は呼び出し元が明示的に渡したフィールド（非 nil / 非空）のみ上書きし、フロントエンドが部分的に
// 変更したドロワーのフォームを送信できるようにする。config 内の未表示フィールドを返送する必要をなくし、
// 「マスク値が実際の秘密キーを上書きする」事故を防ぐ。
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// ここでは意図的に 0 を**一切**加工しない：0 は有効な設定で、「レート制限なし」を意味する。
	//
	// 以前は `if c.RatePerMin <= 0 { c.RatePerMin = デフォルト値 }` と記述し、「未指定時に
	// 安全なデフォルトを与える」意図だったが、「明示的な 0」も取り込んでしまった——ドキュメント、UI の説明と
	// takeTokens はいずれも 0 を制限なしと解釈するのに、ここだけ 20（DingTalk/WeCom/Telegram）
	// または 100（Feishu）に黙って変え、操作者は制限解除と思っても実際は 20/分に制限され、説明もなかった。
	//
	// 「未指定」と「明示的な 0」の違いは呼び出し元だけが分かる（リクエスト本文で省略 vs 明示的な 0）ため、
	// デフォルト値は server 層でフィールド省略時に設定する。notifyCreateChannel を参照。
	if c.RatePerMin < 0 {
		return 0, errors.New("レート制限は0以上にしてください")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled は有効/無効を切り替える。
//
// チャネルを無効にする際は、未送信の配信も skipped にする：そうしないと再度有効にした後に
// 「無効期間中に滞留した」古い脆弱性をまとめて受信し、鮮度がなく新規と誤認されやすい。
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "チャンネルは無効です", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel はチャネルを削除する。配信履歴は外部キーに従って連鎖削除する
// （チャネル設定自体がなくなると、履歴を解釈できない）。
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx は呼び出し元のトランザクション内で通知イベント1件を**ベストエフォートで**書き込む。
//
// 脆弱性書き込み経路の通知関連変更はこれだけ：INSERT 1回のみで、テーブルを読まず、チャネルを扱わず、
// フィルターも実行しない。コミット時点で「脆弱性の保存」と「通知タスクの存在」が原子的に一致し、
// コミット成功後にキュー未投入でメッセージが永久に失われる間隙をなくす。
//
// 次の2つは意図した重要な設計：
//
//  1. **SAVEPOINT を使う理由**：PostgreSQL はトランザクション内の任意の文が失敗すると、全体が
//     aborted 状態になり、以降は全ステートメント（COMMIT も含む）が失敗する。そのため「この INSERT の
//     エラーを無視し、呼び出し元にコミットを続行させる」ことは PG ではできず、セーブポイントでエラーを
//     この文に隔離する必要がある。セーブポイントがなければ「全体をロールバック」しかない。
//
//  2. **全体ロールバックが誤りである理由**：通知は便利機能で、脆弱性記録こそが製品そのもの。通知
//     テーブルの問題（旧 DB の未移行、ディスクの一時障害）で高リスクの脆弱性を保存できなくしてはならない。そのためここでは
//     エラーを隔離し、ログを記録して false を返し、脆弱性書き込みは通常どおりコミットする——代償として当該通知1件を失う。
//     error でなく bool を返すのは意図的：呼び出し元は書き込み成否に影響するエラーとして扱わないこと。
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 通知イベントのシリアライズに失敗 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] セーブポイントの作成に失敗 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 通知イベントの書き込みに失敗 finding=%d（脆弱性の記録には影響しません）: %v", findingID, err)
		// セーブポイントへロールバックし、トランザクションを aborted 状態から回復させる。
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] セーブポイントへのロールバックに失敗 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// セーブポイントを解放し、長いトランザクションに不要なセーブポイントが蓄積することを防ぐ。
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent は InsertNotificationEventTx の独立トランザクション版で、既存の
// トランザクション外の呼び出し箇所で使用する（チャネルの「テストメッセージ送信」など、実際の finding がない場合）。
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("通知イベントのスナップショットのシリアライズに失敗しました：%w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents は未割当の脆弱性イベントを、現在有効なチャネルごとに配信タスクへ展開し、
// 今回処理したイベント数と新規配信数を返す。
//
// 全処理を1トランザクションで実行：イベントは FOR UPDATE SKIP LOCKED で取得し、複数プロセスが同時実行しても
// それぞれ異なる行を取得する（プロジェクトのアーカイブキューも同じ取得手法を使用。以下参照：
// db/task_archives.go の completeNextArchiveJob）。
//
// フィルター照合は意図的に SQL ではなく Go 側に配置：チャネルのフィルター条件は任意フィールド群の JSONB で、
// 6種類の組み合わせ照合を SQL で表現すると保守しにくくなる一方、チャネル数は「手動設定した数件」なので、
// 全件読み込み後にメモリ内で個別照合する方が速く、テストもしやすい。
//
// どのチャネルにも一致しないイベントも fanned_out にする —— そうしないと未割当集合に永久に残り、
// tick ごとに再走査される。
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // コミット成功後は no-op

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// スナップショットは自分たちで書き込むため理論上は解析可能なはずだが、解析失敗で配信フローを止めない。
		// ただしこのイベントは全フィールドが空なので、フィルター条件付きの全チャネルでスキップされる——通知1件を失っても、
		// 不正な1行でキュー全体を停止させない。
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind は行の値を正とする：スナップショット側は描画用コピーで、旧版が書き込んだ可能性がある。
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// 今回のイベントを割当済みにする。どのチャネルにも一致しないイベントも対象（関数コメント参照）。
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx はトランザクション内で有効なチャネルを取得する。数が少ないので、
// ページ分割もキャッシュもしない——キャッシュは「設定変更がいつ反映されるか」という追加のタイミング問題を生む。
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames は資産 id を短い表示名に解決し、通知メッセージに使用する。
//
// 戻り順は引数順と一致し、長さは短くなりうる（存在しない id をスキップ）。引数順を保つのは、
// 同じ脆弱性の複数回配信で資産順序を安定させるため——再試行後のメッセージで
// 資産順序が変わると、「資産が変わった」と誤解される。
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName は資産型ごとに最も識別しやすい情報を選ぶ。
// フォールバックは空文字列で、「名前を取得できない資産」の表示は呼び出し元が判断する——この関数では仮の表示を捏造しない。
// そうしないと「資産#42」のようなノイズが通知メッセージに混在し、読み手が実際のドメインと誤認する。
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify は脆弱性の対応状態を更新し、同じトランザクションで状態変更の
// 通知イベント1件を登録する。
//
// 戻り値 from=変更前の状態；found=脆弱性が存在するか；notified=イベント登録成功か。
//
// 意図した3つの動作：
//   - 状態が実際に変わらない場合はイベントを登録しない。フロントエンドのドロワーから同値を再送したり、自動化スクリプトが
//     冪等に再実行したりしても、通知ノイズを生じさせないこと。
//   - 脆弱性がない場合は found=false を返し、一切書き込まず、呼び出し元が 404 に変換する。
//   - イベント登録失敗は状態更新に影響しない（RecordNotificationEventTx のセーブポイントの説明を参照）ため、
//     notified=false でも状態変更は成功しており、呼び出し元はエラーにしないこと。
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx は**呼び出し元のトランザクション**内で脆弱性状態を更新し、状態変更通知イベントを登録する。
//
// トランザクション単位の関数に抽出し、すべての状態変更経路で同じ意味を共有する——以前は
// patchFinding だけが通知付き版を使用し、**再テストの結論が「修正済み」の場合**（finding_retests
// 内の `UPDATE findings SET status=...`）は直接 DB に書き込んでいたため、
// `on_status_change` を設定したチャネルはこの種の状態遷移の通知を一切受信できず、画面の状態が黙って変わり、
// 運用担当はプラットフォームを開いて初めて気付いていた。
//
// 戻り値 from=変更前の状態、found=脆弱性が存在するか、changed=状態が実際に変わったか、
// notified=イベント登録成功か（登録失敗は状態更新に影響しない。RecordNotificationEventTx を参照）。
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 状態が実際に変わらない場合はイベントを登録しない：同値の再送、冪等な再実行で
		// 通知ノイズを生じさせないこと。
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats は通知ページ上部の概要件数。
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 最古の送信待ち配信から現在までのミリ秒数
}

// NotificationStatsSnapshot は通知システムの健全性を集計する。
// BacklogAgeMS は「通知が停止していないか」の最も直接的な指標で、pending 件数よりはるかに有用。
// 3 件の滞留と 3 件の滞留でも、3 秒から 3 時間までの違いがありうるため。
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
