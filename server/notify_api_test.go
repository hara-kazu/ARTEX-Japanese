package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 通知機能のエンドツーエンド動作：脆弱性の保存 → イベント → 振り分け → 実 HTTP 送信を検証します。
//
// 安全上、このテストは**全体の Notifier.step() を呼ばず**、自分が作った
// 通知先だけで stepRealtime/stepDigest を呼びます。step() は DB の全有効通知先を巡回し、
// 実際の DingTalk/WeCom ボットを設定した開発 DB でテストすると、グローバルな step がテスト中に
// 作成した脆弱性を本当のグループへ送ってしまうためです。通知先別の呼び出しで偽の受信先だけに影響を限定します。
//
// 後処理：テストで作ったイベント（配信を連鎖削除）と通知先を削除し、実際の通知先に蓄積を残しません。
//
// 検証対象：stepRealtime/stepDigest は戻り値がなく内部でログを記録するため、ここでは
// **観測できる外部動作**（偽の受信先の本文、配信行の状態）を検証します。関数の
// 戻り値の代替より実際の呼び出し経路に近くなります。

// notifyFixture はこのファイルの共通テスト装置。
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 自前の task/exploration。脆弱性をここへ保存し、他のテストのデータから分離します。
	taskID int64
	expID  int64
	// cleanupMark より後のイベントもまとめて削除します。
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 偽の受信先はすべて 127.0.0.1 で動きますが、配信は既定でループバックを拒否します
	// （SSRF による同じ端末やクラウドメタデータへの接続を防止）。テストで明示的に許可し、
	// 既定の拒否動作は notify パッケージの ssrf_test.go が検証します。
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// 自前の task を作成します。共通装置 trafficEvidenceServer の task から exploration id を取得できず、
	// 脆弱性の記録にはそれが必須なためです。
	task, err := s.m.CreateTask("通知推送测试", "推送行为验证", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 装置を独立させ、fixture 作成前のイベントを一度に振り分け済みとします。
	//
	// FanOutPendingEvents は**全体対象**で、DB の全未処理イベントを
	// 一致する全通知先へ展開します。共通装置 trafficEvidenceServer 自体も脆弱性を一件記録し、
	// 返却する初期 finding がそれです。他のテストの残存もあり得ます。分離しないと、
	// こうした無関係なイベントがこのテストの通知先へ配られ、「配信 N 件」などの検証が
	// 実行順で成功したり失敗したりし、常時失敗より調べにくくなります。
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("清理通知事件失败: %v", err)
		}
	})
	// 全体スイッチを有効にします（他のテストで無効にした可能性があります）。
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record は本物の証拠保存経路で脆弱性を記録し、finding id を返します。
// **同一トランザクション**で通知イベントを登録する経路で、この機能の接続箇所です。
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 的摘要",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("记录漏洞失败: %v", err)
	}
	return out.FindingID
}

// channel は通知先の設定を読みます（通知先ごとの stepX 呼び出し用）。
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("读渠道失败: %v", err)
	}
	return ch
}

// deliver はイベントを振り分け、指定通知先だけで一回配信します。
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("分派失败: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel は HTTP インターフェースで作成し、入口の検証も確認します。
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("建渠道失败 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("建渠道返回异常: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook は受信した要求本文を記録する偽の受信先。
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("假接收端只收到 %d 条请求，取不到第 %d 条", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("假接收端没有收到任何请求")
	}
	return f.body(t, f.count()-1)
}

// markdownText は本文を取り出し、提供元ごとのフィールド名の違いを扱います。
// DingTalk の markdown は `text`、ActionCard は `text`、WeCom の markdown は `content`。
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("请求体里没有可识别的正文: %v", body)
	return ""
}

// agePendingBatch は待機配信の時刻を古くし、集約バッチの期限到達を試します。
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "实时推送",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL注入", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("应发出 1 条消息，实际 %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL注入", "🟠 高", "要約"} {
		if !strings.Contains(text, want) {
			t.Fatalf("消息正文缺少 %q:\n%s", want, text)
		}
	}
	// 配信は sent へ遷移すること。
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("投递后仍有 %d 条未标记 sent", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "掩码用例",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("列渠道失败 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("接口回显泄露了凭据: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("新建的渠道未出现在列表里")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("凭据字段应为掩码值: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("接口应告知前端哪些字段是凭据")
	}

	// PATCH は名前変更 + マスク済み認証情報だけを返し、元の認証情報をそのまま保持すること。
	body, _ := json.Marshal(map[string]any{
		"name":   "改名后",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("更新失败 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("掩码回传把真凭据覆盖了: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("掩码回传把 secret 覆盖了: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "改名后" {
		t.Fatal("名字未更新")
	}

	// secret の明示的な空欄指定は反映すること（マスクを返すだけなら保持）。
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("清空 secret 失败 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("空串应清空 secret")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"类型非法", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "無効なチャンネルの種類です"},
		{"缺名称", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "チャンネル名がありません"},
		{"缺 webhook", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook 协议非法", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook アドレスが無効です"},
		{"模式非法", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "無効な通知モードです"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("应返回 400，得到 %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("错误信息应提到 %q，得到 %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("删除不存在的渠道应 404，得到 %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "仅严重",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "低危问题", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("低于阈值的漏洞不该产生投递，得到 %d 条", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("被过滤的漏洞不应发出消息")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "汇总推送",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("汇总漏洞%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 期限前は送信しない。
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("汇总批次未到期就发了")
	}

	// 時刻を古くすると三件を一メッセージへ集約。
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("三条应汇总成一条消息，实际发了 %d 条", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "過去") || !strings.Contains(text, "3件の脆弱性") {
		t.Fatalf("汇总消息缺少条数/时间窗文案:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("汇总漏洞%d", i)) {
			t.Fatalf("汇总消息缺少第 %d 条:\n%s", i, text)
		}
	}
	// 同じバッチでは batch_id を共有すること。
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("三条投递应共享一个 batch_id，得到 distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "停用渠道",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "停用期间的漏洞", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("停用渠道不该产生投递，得到 %d 条", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状态变更订阅",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "状态变更用例", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("改状态失败 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 二件あること。fixed は状態変更で、finding_created も同じ回に送る可能性があります。
	// 状態変更は実際には後から作成しますが、順序に依存せず全件で探します。
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "状態の変更") && strings.Contains(text, "修正済み") {
			found = true
		}
	}
	if !found {
		t.Fatalf("没有收到含「状态变更 → 已修复」的消息（共 %d 条）", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "不订阅状态变更",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "不订阅变更", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("改状态失败 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("未订阅状态变更的渠道不该收到状态变更投递，得到 %d 条", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "测试发送",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("测试发送失败 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("假接收端应收到 1 条测试消息，得到 %d", hook.count())
	}
	// テストメッセージは一目でテストと分かり、本当の脆弱性と誤認させないこと。
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "テスト") {
		t.Fatalf("测试消息应标明是测试: %s", text)
	}
	// 設定の不正時は通知先の元のエラーをそのまま返すこと。
	badID := f.createChannel(t, map[string]any{
		"name":   "坏地址",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("投递失败应回 502，得到 %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 必ず失敗するアドレスで failed 配信を作る。
	chID := f.createChannel(t, map[string]any{
		"name":   "失败重试",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "会失败的推送", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 再試行予算を使い切るまで配信。
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("重试耗尽后应为 failed，得到 %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("查历史失败 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("应查到 1 条失败投递，得到 total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("历史里应带上失败原因，否则用户无法排查")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("尝试次数应被记录，得到 %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "会失败的推送" {
		t.Fatalf("历史应带出漏洞标题，得到 %q", hist.Deliveries[0].Title)
	}

	// 手動の再送：pending へ戻り、試行回数をゼロにすること。
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("重发失败 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("重发后应为 pending 且 attempts=0，得到 %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 失败: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta 应列出全部 %d 个渠道，得到 %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("渠道 %s 未上报凭据字段", k.Kind)
		}
	}

	// 三つの全体設定の往復。末尾スラッシュを正規化し、リンクが "//function/..." にならないこと。
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("写设置失败 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("回链地址未规范化: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("汇总周期未生效: %v", payload["notify_digest_interval_min"])
	}

	// 不正な値は拒否すること。
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s 应返回 400，得到 %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL はリンク構築を検証します。public_base_url を設定した場合、
// 一件のメッセージもボタン付き ActionCard を使い、脆弱性詳細へリンクすること。
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "回链",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "带回链的漏洞", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("有回链时应用 ActionCard，得到 msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("回链不对\n期望 %s\n得到 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL は逆の検証。外部アドレスがなければ不正なリンク
// （localhost や相対パスなど）を作らず、単なる markdown へ戻ること。
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "无回链",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "无回链的漏洞", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("未配外部地址时应发 markdown，得到 %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "詳細を表示") {
		t.Fatalf("未配外部地址时不该出现详情链接:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder は「通知なく消える」問題の修正を示すエンドツーエンド検証。
//
// 集約メッセージは通知先の長さ上限（WeCom は 4096 バイト）に従い、一回に入らなければ**項目単位**で分割。
// 含まれた分を到着済みにし、残りを待機列へ戻して次を待ちます。以前は全バッチを
// 成功にしたため、省略された分が本文にも失敗リストにもなく、履歴だけ成功となり、
// 脆弱性を失っていました。
//
// 四点を検証：①実際に入った件数だけを到着済みにする ②残りは待機中 ③後回しの項目は
// **再試行回数を消費しない** ④次の回で残りを送れる（停止しない）。
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// WeCom を使います。markdown の 4096 バイト上限は六通知先中で最も小さいためです。
	chID := f.createChannel(t, map[string]any{
		"name":   "分段汇总",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 長いタイトルで、60 件が確実に 4096 バイトを超えて分割されるようにする。
	longName := strings.Repeat("超长漏洞名称", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("应只发出一条消息，得到 %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("应有条目被标记为已送达")
	}
	if pending == 0 {
		t.Fatalf("一批 %d 条不可能全装进 4096 字节，应有剩余待发；sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("条目数对不上：sent=%d pending=%d total=%d（既没送达也没待发=丢失）", sent, pending, total)
	}
	// 本文で、今回含めなかった残り件数を正確に伝えること。
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "残り") {
		t.Fatalf("消息应说明还有条目未包含在本条:\n%.400s", text)
	}

	// 後回しの項目は再試行予算を消費しないこと。取得時の attempts の仮 +1 を延期時に戻す。
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("被推迟的条目不该消耗重试次数（否则几条之后就会被判失败），得到 attempts=%d", maxAttempts)
	}

	// 収束まで繰り返し、**最終的な全件到着**と実際に複数回に分かれたことを検証。
	// 「二回目で完了」より強く、分割が停止せず、残りも失わないことを示します。
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("分段投递不收敛：跑了 %d 轮仍有 %d 条悬而未决", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("第 %d 轮没有任何进展，剩余 %d 条会永久卡住", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("一条 4096 字节的消息装不下 %d 条长标题漏洞，应分多轮发出，实际只用了 %d 轮", total, rounds)
	}
	// 初回後はすべて**残りの続送だけ**で、通知先が拒否した項目がないこと。
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("假接收端始终返回成功，不该有失败条目，得到 %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget は設定間のずれを防ぐ検証。
//
// 再試行予算（db.MaxNotifyAttempts）とバックオフ列（notifyBackoff）は別パッケージにあり、
// 前者は状態機械の方針、後者はエンジンの実行間隔です。予算だけ 5 回へ上げ、
// バックオフを追加し忘れると、エラーにならず、第 4、5 回も最後の間隔を使い、
// 「再試行が理由なく遅くなる」ように見え、ここが原因とは気付きにくくなります。
// 両者の長さ一致を検証し、CI でずれを検出します。
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("退避档位数(%d)与最大尝试次数(%d)不一致——改一个必须同时改另一个",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// バックオフは単調非減少とし、再試行の加速による制限の悪化を防ぎます。
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("退避间隔必须单调不减：第 %d 档 %v < 第 %d 档 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget は「トークン取得が先、配信取得が後」の順序を固定。
// 逆順（取得後に破棄）では制限された配信にも attempts が加算され、
// 単なる待機で予算を使い切り failed になります。
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// バケット単体を検証し、Server は不要で、そのために作ってもいけません。
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 一分に 1 件：満杯でも最大 1 件。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("满桶时每分钟 1 条应取 1 个令牌，得到 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("令牌耗尽后应立即返回 0，得到 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("半程不应补满一个令牌，得到 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("满一个周期应补回 1 个令牌，得到 %d", got)
	}
	// 無制限の通知先にも有限の上限を使い、一回の処理が無限の蓄積で停止するのを防ぎます。
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("不限流应返回每轮上限 %d，得到 %d", notifyUnlimitedBurstPerTick, got)
	}
	// 通知先ごとのバケットは独立。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("渠道 1 的桶应仍然为空，得到 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens は「want 個だけ取得」を固定。
//
// 以前は全トークンを取り出してから呼び出し元で制限したため、rate=100/min の満杯の通知先で、
// 一回に 5 件使い、残り 95 トークンを捨てていました。待機配信がない回でも消費していました。
// このため「蓄積時には一回に rate_per_min 件送れる」というコメントの動作を実現できませんでした。
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 初期は満杯（100）、今回必要なのは 5 個だけ。
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5 时应恰好取 5 个令牌，得到 %d", got)
	}
	// 重要な検証：残り 95 個は破棄されずバケットに残ること。
	// 時刻を進めず、補充分ではなく既存分だけを取得させます。
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("剩余令牌应仍可取用（期望 95），得到 %d——桶被整轮抽空了", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("桶已取尽，应返回 0，得到 %d", got)
	}
	// want<=0 では消費しないこと（空の回は消費しない）。
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 应返回 0，得到 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 的那次不该消耗令牌，应仍可取满 20，得到 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget は集約の二つの単位を固定。
//
// バッチサイズを要求予算に結び付けると、rate_per_min=20 の通知先は各
// 3 秒 tick で 1 トークンしか補充できず、一メッセージに脆弱性 1 件しか入らなくなります。
// 実質的に集約しないのに「直近 30 分で 1 件追加」と表示されます。この退化はエラーにならず、
// 既存のエンドツーエンド検証も、十分大きい limit を stepDigest へ手動で渡して
// step の枠計算を回避するため検出できません。ここで判断自体を直接検証します。
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 一バッチ = 一メッセージ = 一要求 = 一トークン。トークンの単位はメッセージで、脆弱性ではありません。
	if tokens != 1 {
		t.Fatalf("汇总一批只发一条消息，应恰好消耗 1 个令牌，得到 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("汇总批次大小应为内存上界 db.MaxDigestBatchSize=%d，得到 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 重要な関係：バッチサイズは各回の要求予算より十分大きいこと。同程度になれば、
	// 「メッセージ件数」と「一バッチの脆弱性件数」を再び混同しています。
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("汇总批次大小 %d 不应受每轮请求预算 %d 约束——"+
			"请求预算是由租约倒推的「发几次请求」，与「一批装几条漏洞」是两个量纲",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease も設定間のずれを防ぐ検証。
//
// 通知先ごとの一回の上限（notifyMaxSendsPerChannelPerTick）はリース期間から逆算します。
// 直列配信の最悪時間はリース未満とし、そうでなければ最後まで送る前に期限が切れ、
// 複数インスタンスでは相手が再取得して重複配信します。三定数は別の場所にあり、
// 一つの変更でエラーなしに関係が崩れ得るため、ここで固定します。
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("单渠道一轮的最坏耗时 %v 不应达到或超过租约 %v"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"改这三个常量中的任意一个都要同步检查另外两个",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
