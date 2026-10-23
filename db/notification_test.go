package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// このファイルのケースは実際に PostgreSQL に接続する（DB がなければスキップ）。これらの SQL は
// FOR UPDATE SKIP LOCKED、make_interval、JSONB、複数行の IN(...) プレースホルダー連結を使用し、
// 「コンパイルは通るが実行時に失敗しうる」記法なので、実行しないと検証したことにならない。

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel はチャネルを1つ作成し、テスト終了時に自動削除する。
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "测试渠道-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("チャネルの作成に失敗しました: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent はイベント1件を直接書き込み（finding を経由しない）、割当と配信のテストに使用する。
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("イベントの書き込みに失敗しました: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 3種類の資産はそれぞれ表示基準が異なる：ドメイン、IP、URL。
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 入力順は意図的にランダムとし、存在しない id も1つ含める。
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("資産名の解析に失敗しました: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("資産名の数が一致しません。期待値 %v、実際の値 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("順序/値が一致しません。期待値 %v、実際の値 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure はセーブポイント機構の中核ケース：
// トランザクション内で notification_events への書き込みを必ず失敗させる（一時的に常に false の制約を追加）。
// ① 当該関数が false を返すこと ② トランザクションが aborted にならず、後続文が実行可能なことを検証する。
//
// セーブポイントがなければ PostgreSQL はトランザクション全体を無効にし、以降のどの文も
// "current transaction is aborted" で失敗する——これがまさに「通知テーブル1つの問題で
// 脆弱性を保存できない」という障害経路。
//
// ここでは意図的に **COMMIT でなく ROLLBACK で終了**する：PG の ALTER TABLE はトランザクションに含まれるため、
// コミットすると一時制約が schema に永久に残り、後続の全ケースも失敗させる。
// ロールバックは DDL を自動的に取り消すので、手動削除は不要。アサーションは「トランザクションが生きていること」だけを求め、
// 実際のコミットは不要。
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 防御的な削除：過去の実行がこの制約を残していた場合は、先に取り除く。
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 一時制約を取り消す。関数コメント参照

	// NOT VALID：以降に書き込む行のみ制約し、DB に既存の履歴イベントは検証しない
	// （既存行が違反していると制約を追加できないため）。
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("一時制約の追加に失敗しました: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("必ず失敗する制約の下で、書き込み成功が報告されました")
	}
	// 重要なアサーション：トランザクションが引き続き利用可能。
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("トランザクションが汚染されています（セーブポイントが機能していません）: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("ロールバックに失敗しました: %v", err)
	}
	// DDL がロールバックで取り消されたことを確認し、後続ケースに問題を残さない。
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("一時制約がロールバックで取り消されておらず、後続のテストケースを汚染します")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL注入"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("振り分けに失敗しました: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"全件受信するチャネルで high を受信", highSQL, all.ID, true},
		{"全件受信するチャネルで low を受信", lowXSS, all.ID, true},
		{"重大のみのチャネルで high をスキップ", highSQL, onlyCritical.ID, false},
		{"重大のみのチャネルで critical を受信", criticalXSS, onlyCritical.ID, true},
		{"SQL のみのチャネルで SQL を受信", highSQL, sqlOnly.ID, true},
		{"SQL のみのチャネルで XSS をスキップ", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("配信の存在有無: 期待値 %v、実際の値 %v", tc.want, exists)
			}
		})
	}

	// 再度割り当てても配信が重複しないこと（fanned_out は冪等）。
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("振り分け済みのイベントは再処理されるべきではありません。実際の値: events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel は「イベントがどのチャネルにも一致しない」場合を検証する。
// この種のイベントも割当済みにしないと、未割当集合に永久に残り、tick ごとに再走査される。
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["绝不匹配的类型"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("配信は生成されるべきではありません。実際の件数: %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("どのチャネルにも一致しないイベントも振り分け済みにする必要があります。そうしないと無限に再走査されます")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// リアルタイム取得では realtime チャネルの1件のみ取得し、digest チャネルには触れないこと。
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗しました: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1 件取得するべきです。実際の件数: %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("取得後は sending かつ attempts=1 であるべきです。実際の値: state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// 結合読込した描画コンテキストが完全であること（チャネル設定 + イベントスナップショット + finding id）。
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("取得結果にチャネル設定がなく、描画に失敗します")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id がイベントから引き継がれていません。実際の値: %d", got[0].FindingID)
	}

	// リース期限前なので、2回目の取得は空となること——これが「同じ行を2つの dispatcher が同時に配信しない」
	// 保証となる。
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("リース期間中は重複して取得するべきではありません。実際の件数: %d", len(again))
	}

	// digest チャネルの配信にはリアルタイム取得で触れないこと。
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("リアルタイムの取得では digest チャネルの配信を取得するべきではありません。実際の件数: %d", len(left))
	}
}

// TestClaimExpiredLeaseRecovers はクラッシュからの自己回復を検証：配信中のプロセス停止で sending 行が
// 残るが、リース期限後に再取得できないと、この配信は永久に止まる。
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("初回の取得に失敗しました: %v (%d 件)", err, len(first))
	}
	// リースを手動で過去に移し、「リース切れ」を再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("リース期限切れの sending 行は再取得できるべきです。実際の件数: %d", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("再取得では試行回数を加算するべきです。実際の値: %d", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 無効化は既存の送信待ち配信も skipped にする。
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("無効にしたチャネルの既存の未送信配信は skipped になるべきです。実際の値: %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("無効なチャネルは取得できるべきではありません。実際の件数: %d", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// バッチ作成直後で経過時間が 0 なら、30 分の周期では期限に達しないこと。
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("バッチの期限判定に失敗しました: %v", err)
	}
	if due {
		t.Fatal("作成直後のバッチはすぐに期限に達するべきではありません")
	}

	// 3件の配信の作成時刻をともに古くし、周期分蓄積したバッチを再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("周期を超えたバッチは期限に達したと判定するべきです")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("集約バッチの取得に失敗しました: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("集約では全 3 件を一度に取得するべきです。実際の件数: %d", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("集約バッチには batch_id を記録する必要があります。そうしないと、これらがまとめて送信されたことを履歴で判別できません")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("同じバッチでは batch_id を共有するべきです。実際の値: %v vs %d", dl.BatchID, firstBatchID)
		}
	}

	// このバッチを**全体で**失敗させて再スケジュール後に取得しても、batch_id は元の値を保持すること（COALESCE の効果）：
	// そうしないと1回の再試行で「このバッチは一緒に送った」という事実が消える。
	//
	// 1件だけでなくバッチ全体を再スケジュールする必要がある——配信エンジンの集計メッセージ送信時もこのように処理する
	// （1メッセージがバッチ全体を表し、成否を共有する）。1件だけ再スケジュールすると、他はまだリース期間中で、
	// 再取得では当然その1件しか取れない。
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "模拟失败"); err != nil {
		t.Fatal(err)
	}
	// リースを過去に移し、バックオフ時間の到達を再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("再取得では全 3 件を取得するべきです。実際の件数: %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("再試行後も batch_id は元の値 %d を保持するべきです。実際の値: %v", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("取得に失敗しました: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "网络抖动"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "网络抖动" {
		t.Fatalf("再スケジュール後は pending となり、理由を記録するべきです。実際の値: state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "重试耗尽"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed であるべきです。実際の値: %s", state)
	}

	// 手動再送は再試行回数をゼロにし、直ちに期限到達とすること。そうしないと過去の失敗予算を引き継ぐ。
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("再送に失敗しました: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("再送後は pending かつ attempts=0 であるべきです。実際の値: state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("再送はすぐに取得できるべきです")
	}

	// 配信済みの配信は再送できないこと。
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("送達済みの配信は再送を許可するべきではありません")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "分页测试"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("検索に失敗しました: %v", err)
	}
	if total != 5 {
		t.Fatalf("総数は 5 であるべきです。実際の値: %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("1 ページ 2 件であるべきです。実際の件数: %d", len(page1))
	}
	// 新しい順：1ページ目の先頭 id は2ページ目の先頭より大きいこと。
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("ページの順序は新しいものが先であるべきです。実際の値: page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// 描画コンテキストを履歴とともに返すこと。そうしないと一覧に「何を通知したか」を表示できない。
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("履歴項目に表示フィールドがありません: %+v", page1[0])
	}

	// 状態フィルター：pending は存在しない。
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending の配信は存在するべきではありません。実際の件数: %d (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("通知状态变更测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "状态变更用例",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 保存時に finding_created イベント1件を登録済みなので、先に数えて基準値にする。
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("脆弱性の保存と同じトランザクションで通知イベントを 1 件登録するべきです")
	}

	// 同じ状態への変更：イベントを生成しないこと（再送による通知ノイズを防ぐ）。
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("状態の設定に失敗しました: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("状態が変化しない場合は通知イベントを登録するべきではありません")
	}
	if from != "pending" {
		t.Fatalf("変更前の状態 pending を返すべきです。実際の値: %q", from)
	}

	// 実際の変更：イベントを登録し、from/to を記録すること。
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("状態の設定に失敗しました: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("状態が実際に変化した場合は通知イベントを登録するべきです")
	}
	if from != "pending" {
		t.Fatalf("from は pending であるべきです。実際の値: %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("状態変更イベントが見つかりません: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("スナップショット内の状態遷移が正しくありません: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットに描画用フィールドを含めること。そうしないと状態変更メッセージが空になる。
	if snap.VulnClass != "SQL注入" || snap.Severity != "high" || snap.Name != "状态变更用例" {
		t.Fatalf("スナップショットに描画フィールドがありません: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("状態は fixed に更新済みであるべきです。実際の値: %s", status)
	}

	// 存在しない脆弱性：found=false、エラーにしない。
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("存在しない脆弱性は found=false をエラーなしで返すべきです。実際の値: found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("集計に失敗しました: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("チャネル数が正しくありません: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("未送信の配信が集計されるべきです: %+v", stats)
	}
	// 作成直後の配信の滞留経過時間は 0 付近であり、負数や巨大値でないこと。
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("滞留時間が不正です: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 往返",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("作成に失敗しました: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("読み込みに失敗しました: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 往返" {
		t.Fatalf("往復後のフィールドが一致しません: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("デフォルトで有効であるべきです")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("設定が正しく保存されていません: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("フィルター条件が正しく保存されていません: %+v", filter)
	}

	// 更新後に再読込。
	got.Name = "改名了"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "改名了" || after.IsEnabled() {
		t.Fatalf("更新が反映されていません: %+v", after)
	}

	// 削除後は黙って成功とせず「存在しない」と報告すること。
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("ErrNotificationChannelNotFound を期待しましたが、実際の値は %v です", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("重複削除では存在しないというエラーを返すべきです。実際の値: %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate は過去の誤実装箇所を固定する：
// **0 は有効な設定で「レート制限なし」を意味し、db 層が「未指定」としてデフォルト値で上書きしてはならない**。
//
// 過去の bug：SaveNotificationChannel に `if RatePerMin <= 0 { デフォルト値を取得 }` と記述したため、
// ドキュメント、UI の説明、takeTokens はいずれも「0=制限なし」と解釈するのに、保存層だけ黙って
// 20（DingTalk/WeCom/Telegram）または 100（Feishu）に変更し——操作者は制限解除と思っても実際は制限され、
// 説明もなかった。「未指定」と「明示的な 0」の違いはリクエスト本文だけが表現できるため、
// デフォルト値は server 層で設定し（notifyCreateChannel を参照）、db 層は保存のみ担当する。
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 明示的な 0（レート制限なし）：そのまま保存すること。
	unlimited := &NotificationChannel{
		Name: "不限流", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("明示的な 0 はレート制限なしを意味し、そのまま保存する必要があります。実際の値: %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("デフォルトのモードは realtime であるべきです。実際の値: %s", got.Mode)
	}

	// 負の値は不正入力で、黙って別値に変更せず拒否すること。
	bad := &NotificationChannel{
		Name: "负限流", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("負のレート制限は拒否されるべきです")
	}
}

// TestDeleteChannelCascadesDeliveries は外部キーの動作を固定：チャネル削除後は配信履歴も消える
// （設定自体がないと履歴を解釈できない）が、イベント自体は残すこと——他のチャネルが参照している可能性がある。
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("前提条件が成立しません: 配信が生成されていません")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("チャネルの削除後は配信も連鎖削除されるべきですが、まだ %d 件あります", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("チャネルの削除でイベント自体も削除するべきではありません")
	}
}

// TestClaimDigestBatchHonorsCallerLimit は監査で指摘された抜けを検証する：
// 集計チャネルは以前トークンバケットを完全に迂回していた——allow は takeTokens で差し引かれても使用されず、
// rate_per_min は digest モードで作用しなかった。現在は limit も制約に参加する。
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3 を指定：3 件だけ取得し、残りは DB に残す。
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗しました: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("呼び出し元のレート制限枠に従い、3 件だけ取得するべきです。実際の件数: %d", len(got))
	}
	// limit=0 は今回の割当枠を使い切った意味：1件も取得せず、エラーにもしないこと。
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("枠が 0 の場合は 0 件をエラーなしで取得するべきです。実際の件数: %d err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange は監査で指摘された整合性の欠落を検証する：
// 再テストの結論が「修正済み」の場合、状態は変わるが、その UPDATE は直接 DB に書き込み、
// 通知付き版を迂回していたため、on_status_change を設定したチャネルはこの状態遷移の
// 通知を全く受信できず、画面の状態が黙って変わり、運用担当はプラットフォームを開かないと気付かなかった。
//
// このケースは「すべての状態変更経路で状態変更イベントを登録する」を固定する。
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("复测推送测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "复测目标",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 再テスト記録を1件作り、直接完了状態へ進める。
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "复核")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("再テストは 1 つのセッションに関連付けるべきです")
	}
	// 再テストは先に running に入ってから結論を保存すること（実際のフローと一致）。
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("再テストの開始に失敗しました: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "已修复", "证据"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("再テストの終了に失敗しました: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("再テストで修正済みと判定した後は状態が fixed であるべきです。実際の値: %s", status)
	}

	// 重要なアサーション：状態変更イベントが1件存在し、from/to が正しいこと。
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("再テストで修正済みと判定した場合は状態変更の通知イベントを登録するべきです（そうしないと on_status_change を設定したチャネルで受信できません）: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("スナップショットの状態遷移が正しくありません: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットに描画用フィールドを含め、通知が空になることを防ぐ。
	if snap.Name != "复测目标" || snap.Severity != "high" {
		t.Fatalf("スナップショットに描画フィールドがありません: %+v", snap)
	}
}
