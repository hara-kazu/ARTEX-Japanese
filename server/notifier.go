package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 全体設定のキー（settings キー値テーブルに保持し、テーブル追加は不要）。
const (
	// settingNotifyEnabled は通知の全体スイッチ。既定は有効で、保守時に一括停止するためのもので、
	// 機能の有効条件ではありません。実際の有効条件は「通知先の設定があるか」です。
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL は脆弱性の詳細へのリンクを作る外部アクセス先
	// （例：https://artex.example.com）。空欄ならメッセージにリンクボタンを付けません。
	// 既存の外部アドレス設定を再利用できないため、ここに追加します。
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes は集約モードの周期（分）。
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick は配信エンジンのポーリング間隔。3 秒がエンジンの即時性の上限で、
	// 「脆弱性の DB 保存」から「IM への到着」までの遅延の主因です。
	notifyTick = 3 * time.Second
	// notifyLease は配信取得時のリース期間。一回の最悪配信時間より十分長くする必要があり、
	// notify パッケージの HTTP クライアントは 15 秒でタイムアウトします。短いと同じ行を二つの
	// dispatcher が同時に配信します。
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick は各回に振り分けるイベント数を制限し、通知先の初回有効化時に
	// 過去の蓄積を一度にすべて配信タスクへ展開するのを防ぎます。
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes は集約周期の既定値。
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick は通知先にレート制限がない場合の各回の配信上限。
	// 「無制限の通知先 + 一回で千件以上の脆弱性」が、
	// 一回のループを長時間停止させるのを防ぎます。
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick は通知先ごとの一回の配信件数上限。
	//
	// この上限は**リース期間**から逆算します。取得時に行へリース（notifyLease = 3 分）を付け、
	// 直列配信の件数が多く最悪所要時間がリースを超えると、後半の数件を配信し終える前に期限が切れます。
	// 単一プロセスでは問題ありません（Run は単一 goroutine の直列処理で tick は再入しない）が、**二つの
	// プロセスが同じ DB へ接続**すると、相手が期限切れの行を再取得して重複配信し、
	// attempts を二重加算して、元のプロセスの配信中に失敗と判定します。
	//
	// 値：3 分のリース / 一回 30 秒の上限 = 6 は**リースを使い切って**余裕がないため、
	// 使用できません。5 なら最悪 150 秒で 30 秒の余裕を確保します。この関係は
	// TestNotifyTickBudgetFitsWithinLease で固定し、notifyLease、
	// notifySendTimeout、この値のいずれかを変えると検証が失敗します。
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout は一回の配信タイムアウトで、前の定数の値も決定します。
	// 両者の積は notifyLease を超えてはいけません。TestNotifyTickBudgetFitsWithinLease を参照。
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff は失敗時の再試行のバックオフ列で、添字は試行済み回数。
// 初回を含む 3 回の機会は db.MaxNotifyAttempts と対応し、両者を同時に変更する必要があります。
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier は脆弱性通知の配信エンジン。
//
// Scheduler と並ぶ独立 goroutine として動きます（server.New を参照）。意図的に
// Scheduler の tick を再利用しません。通知の即時性（3 秒）はトリガーの周期と異なり、
// 互いの失敗も独立させ、通知の停止が agent のトリガーへ影響しないようにします。
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu は buckets を保護します。通知先が少なく競合も少ないため、一つのロックで十分で、
	// より細かい構造を導入する必要はありません。
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket は一通知先のトークンバケット。
//
// 「一分ごとの件数をゼロに戻す」移動窓ではなくバケットを使うのは、窓の境界の影響が大きいためです。
// 窓の終わりに 20 件、直後に 20 件送ると、プラットフォームには一秒で 40 件となり、
// 制限されます。バケットは一定の速度で補充し、この集中を防ぎます。
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run は ctx の終了までループします。server.New が一度だけ起動します。
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step は一回の処理。新しいイベントの振り分け後に期限が来たタスクを配信します。
//
// どの段階の失敗もログだけを記録して続行し、通知障害をプロセス全体の問題にしません。
// 各 tick は独立しており、次回に自然に再試行します。
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] イベントの振り分けに失敗しました: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] チャネルの読み込みに失敗しました: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// バケットの単位は**メッセージ数**（HTTP 要求数と等価）であり、脆弱性数ではありません。
		// 即時モードでは一致します（一脆弱性に一メッセージ）。集約モードは複数の脆弱性を
		// 一メッセージへまとめるため、トークンを一つだけ消費します。
		//
		// 両モードとも先にバケットを確認し、枠に応じて取得します。逆順だと制限で送れない
		// 配信も再試行回数を消費してしまいます。
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan は今回の集約通知先のトークン消費数とバッチサイズ上限を返します。
//
// 戻り値の二つは**異なる単位**であり、それが独立関数にする理由です。
//
//   - tokens はメッセージ数。脆弱性バッチを一メッセージへまとめ、一回 HTTP 要求するため、常に 1。
//     このため rate_per_min は digest にも適用されます（一分の集約メッセージ数上限）。
//   - claimLimit は今回まとめる脆弱性の最大数。メモリー上限だけに制約され、要求予算とは無関係。
//
// 以前は rate_per_min を digest へ適用するため、各回の要求予算
// （リースから逆算した notifyMaxSendsPerChannelPerTick）をそのままバッチサイズにしていました。
// rate_per_min=20 の通知先では 3 秒の tick に 1 トークンしか補充されず、各集約
// メッセージに脆弱性が 1 件しか入らず、digest が「集約文付きの即時通知」へ退化していました。
// 受信内容は「直近 30 分で脆弱性 1 件追加」の連続となり、db.MaxDigestBatchSize へは到達しません。
//
// この症状はエンドツーエンドテストでは見つけにくく（既存のケースは十分大きい limit を手動で
// stepDigest へ渡して step の枠計算を回避）、ここへ判断を集約し、
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget で直接検証します。
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime は一通知先の即時タスクを取得して配信します。一脆弱性に一メッセージ。
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] リアルタイム配信の取得に失敗しました channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("チャンネルの種類 %q は未登録です", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 描画失敗はローカルデータの問題で、再試行しても改善しません。
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest は期限に達した一通知先の待機配信を、一つのメッセージへ集約して送ります。
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 集計バッチの判定に失敗しました channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 集計バッチの取得に失敗しました channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("チャンネルの種類 %q は未登録です", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// スナップショットの破損で含められなかった配信は、明示的に失敗とします。そうしないと
	// included に入らず、メッセージにも失敗リストにもなく、送信成功後の
	// 一括処理から漏れ、期限まで sending に残って取得を繰り返してしまいます。
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "イベントのスナップショットを解析できないため、この脆弱性をメッセージに表示できません"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 壊れたスナップショットの配信状態の記録に失敗しました channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] スナップショットを解析できない %d 件の配信をスキップしました channel=%d", len(skipped), ch.ID)
	}
	// メッセージへ入った分だけを send へ渡します。included[i] と msg.Items[i] は厳密に対応し、
	// send はそれを使って「通知先に先頭 K 件が入った」を正しい配信行へ反映します。
	n.send(ctx, channel, cfg, msg, included)
}

// send は配信し、結果に従って状態を遷移させます。
//
// 同じバッチ（集約時は数十件の場合もある）で送信結果を共有し、全件到着か全件再試行です。
// 個別の再試行はしません。一メッセージの一部だけを再送するとバッチの意味が崩れるためです。
//
// 唯一の例外は**通知先の長さ上限による分割**。実際に先頭 K 件だけが入った場合は、
// K+1 件目以降を次のバッチへ残し、一緒に成功と記録しません。そうしないと切れた
// 脆弱性がメッセージにも失敗リストにもなく、完全に消えてしまいます。
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 一回の配信に上限を設け、一通知先の停止が残りすべてを止めないようにします。
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 通知先の報告件数が配信数を超えるのは描画層の計算ミスです。その場合も
			// 全件到着と扱って問題を記録する方が、記録の不整合より安全です。
			log.Printf("[notify] チャネルが返した配信済み件数 %d が配信件数 %d を超えています channel=%s。すべて配信済みとして処理します",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 配信済み状態の記録に失敗しました channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 今回のメッセージが長さ上限に達したため、残りを即座に待機列へ戻し、次の tick で続けます。
			// RescheduleDeliveries ではなく DeferDeliveries を使います。失敗ではないため、
			// 再試行予算は消費しません（取得時の仮 +1 をそこで戻します）。
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("このメッセージはチャネルの長さ上限に達したため、先頭の %d 件のみ配信しました。残りは次のバッチで配信します", delivered)); err != nil {
				log.Printf("[notify] 分割した続きの配信のキューへの追加に失敗しました channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 通知先がエラーも到着件数も返さなければ、失敗としてバックオフし、
		// 同じ配信を取得し続けて記録できなくなるのを防ぎます。
		err = fmt.Errorf("チャンネルが配信数を返していません（delivered=%d）", delivered)
	}

	// 失敗時の処置は**各件ごと**に決め、バッチの最大試行数では判定しません。
	//
	// 以前は `if maxAttempts(deliveries) >= MaxNotifyAttempts` で全件を失敗にしましたが、
	// 各試行数は異なります。二回再試行済みの古い配信（attempts=2）が、同じバッチの
	// 新規配信（attempts=1）まで failed にし、新しい脆弱性が再試行なしで永久に失われ、
	// 「古い行に新しい行を巻き込ませない」という目的に反していました。
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 失敗状態の記録でエラーが発生しました channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("%d 回再試行しても失敗しました: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 失敗状態の記録でエラーが発生しました channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 遅延別に再配置します。バックオフは 3 段階でグループも少なく、各件に一回ずつ
	// UPDATE する必要はありません（500 件なら 500 往復になるため）。
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 配信の再スケジュールに失敗しました channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 配信に失敗しました channel=%d kind=%s 永続的な失敗=%d 再試行の上限到達=%d 再試行待ち=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries は all のうち keep にない分を返します（ポインターの同一性で比較）。
// 「メッセージに入らない」配信を見つけ、曖昧なまま放置せず明示的に処理するためです。
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt は通知先の実装を取得し、設定を解析します。
// ok=false は未登録の種類を意味し、無限に再試行せず直接失敗とします。
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 設定の解析失敗時は空の map を渡します。通知先の Validate が「不足する項目」を報告するため、
		// JSON の解析エラーよりユーザーが修正しやすいからです。
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle は一件の脆弱性メッセージを描画します。
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch は集約メッセージを描画し、スナップショットを個別に解析します。一件の破損はその件だけ省略し、
// バッチ全体を失わないようにします。
//
// 戻り値 included と msg.Items は**厳密に一対一**です（第 i 配信 ↔ 第 i 項目）。
// 呼び出し元は「先頭 K 件が入った」という報告で先頭 K 配信を
// 到着済みとするため、この対応は必須です。破損分を省略しても included から除かなければ、
// 添字がずれ、失敗すべき破損項目が到着済み、正常な項目が未到着と誤認されます。
// 破損分は呼び出し元が明示的に失敗とします。stepDigest を参照。
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 破損スナップショットはメッセージにも included にも入れず、呼び出し元が処理します
			// （到着済みに混ぜず、明示的に失敗とします）。
			log.Printf("[notify] 集計バッチ内の解析できないスナップショットをスキップしました delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("まとめた%d件の配信をすべて解析できません", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor はイベントのスナップショットを通知項目へ描画し、資産名と詳細リンクも解析します。
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 資産名を読めない方が通知を受け取れないよりはるかに軽微なので、名前の解析失敗は通知を止めず、
		// メッセージ内の資産が一行減るだけとします。
		log.Printf("[notify] 資産名の解析に失敗しました finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 詳細ページの経路は web/src/app/(main)/function/findings/detail/page.tsx を参照し、
		// query 引数 id から脆弱性 id を読みます。
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens は通知先のバケットから**最大 want 個**を取得し、実際の取得数を返します。
//
// 一トークン = 一メッセージ（一回の HTTP 要求）。即時モードでは必要な件数を渡し、
// 集約モードは一バッチに一メッセージなので 1 を渡します。
//
// 容量は通知先の一分上限で一定速度で補充します。ratePerMin<=0 は無制限で、
// 有限だが十分大きい値を返し、無限の蓄積が一回の処理を停止させないようにします。
//
// want の上限は必須です。なければ全トークンを取り出しますが、呼び出し元にも各回の上限があり、
// 余分なトークンは使えず、補充前に消えます。蓄積した集中送信用の容量を使えず、
// 待機配信がない回でさえ消費することになります。
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 実経過時間に応じ、毎秒 ratePerMin/60 を補充。
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 小さい epsilon を加えて整数化します。浮動小数点の加算では、二回で満たす際に
	// 0.5 + 0.5 が 0.9999999999 となり、int() が 0 へ切り捨てる可能性があり、
	// 満杯のはずでも取得できません。1e-9 は一トークンより十分小さく、本当の不足は通しません。
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled は全体スイッチを読みます。
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL は詳細リンク用の外部アドレスを返し、末尾のスラッシュを除きます。
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval は集約周期を返し、不正または未設定なら既定値を使います。
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot は配信に対応するイベントのスナップショットを解析します。
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("配信%dのイベントスナップショットが空です", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("配信%dのイベントスナップショットの解析に失敗しました：%w", dl.ID, err)
	}
	if snap.Kind == "" {
		// イベントの種類はイベント行を正とし、古い版が書いた可能性のあるスナップショット側は使いません。
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
