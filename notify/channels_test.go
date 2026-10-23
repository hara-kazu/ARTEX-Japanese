package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg は引用符と改行を含む単発送信用のメッセージを作る。`"` と `\n` を含むタイトル/要約を意図的に使う：
// テンプレート補間で不正な JSON を最も生成しやすい入力であるため。
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `登录处 "SQL注入" 风险`,
			VulnClass: "SQL注入",
			Severity:  "high",
			Summary:   "参数 id\n未过滤 导致注入",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg は集約メッセージのバッチを作る。
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "漏洞" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "反射型跨站脚本",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost はダミーの受信側を起動し、受信したリクエストボディとヘッダーをアサーション関数に渡す。
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("リクエスト本文が有効な JSON ではありません: %v\n原文: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("戻り先リンクがある場合は actionCard を送信するべきです。実際の値: %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("戻り先リンクが失われています: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("集約メッセージは markdown で送信するべきです。実際の値: %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "過去30分間") {
			t.Errorf("集約本文に時間範囲がありません: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent は「HTTP 200 だが errcode が 0 でない」場合の判定を固定する。
// errcode を確認しないと配信失敗を成功として記録してしまう。これは各中国国内 IM プラットフォームに共通する落とし穴。
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode が 0 以外の場合はエラーを返すべきです")
	}
	if !IsPermanent(err) {
		t.Fatalf("キーワードの不一致は設定エラーであり、恒久的な失敗とするべきです。実際の値: %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("エラー情報にはプラットフォームのエラーコードを含めるべきです。実際の値: %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("切り詰め後に有効な UTF-8 ではありません——WeCom はメッセージ全体を拒否します")
		}
	})
	// 4096 バイトを確実に超える、十分に長い中国語の集約バッチを作る。
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("本文の %d バイトが WeCom の上限 %d を超えています", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("本文が空です")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 はスライディングウィンドウのレート制限であり、再試行できるべきです。実際の値: %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000 は key が無効であり、再試行では復旧しないため、恒久的な失敗であるべきです。実際の値: %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("インタラクティブカードを送信するべきです。実際の値: %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high レベルの配色は orange であるべきです。実際の値: %v", header["template"])
		}
		// secret 設定時は署名パラメーターが必須で、なければ Feishu が 19021 で拒否する。
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("署名パラメーターがありません: %v", body)
		}
		// カード要素に、url が脆弱性詳細を指すボタンが含まれること。
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("カードに詳細ページへのボタンがありません")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret が未設定の場合は署名パラメーターを含めるべきではありません: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML 解析モードを使用するべきです。実際の値: %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// タイトルと要約はテスト対象/モデルの出力に由来する、信頼できない内容。
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML がエスケープされておらず、インジェクションが可能です: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("エスケープ済みの実体を期待しましたが、実際の値は %q です", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& がエスケープされていません。実際の値: %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 は再試行できるべきです。実際の値: %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 は設定の問題であり、恒久的な失敗であるべきです。実際の値: %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// これがデフォルトテンプレートの存在理由：タイトルに引用符と改行が含まれる場合、単純な
	// `"title": "{{.Title}}"` という書き方は必ず不正な JSON を生成する。{{json .}} なら問題ない。
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 高] 登录处 "SQL注入" 风险` {
			t.Errorf("タイトルが正しく復元されていません: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items の数は 1 であるべきです。実際の数: %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "参数 id\n未过滤 导致注入" {
			t.Errorf("要約が正しく復元されていません: %v", it["summary"])
		}
		// 数値は文字列ではなく JSON 数値でなければならない（json:"...,string" のような書き方でこの問題が起きる）。
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id は数値であるべきです。実際の型: %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("カスタムヘッダーが失われています: %v", r.Header)
		}
		if body["msg"] != "3 条" {
			t.Errorf("カスタムテンプレートの描画が正しくありません: %v", body["msg"])
		}
		if body["first"] != "漏洞1" {
			t.Errorf("range の抽出が正しくありません: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d 条" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("描画結果が JSON でない場合は恒久的な失敗であるべきです（テンプレートの誤りであり、再試行は無意味です）。実際の値: %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("%d 番目の設定は拒否されるべきです: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("メールの組み立てに失敗しました: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From ヘッダーが正しくありません:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To ヘッダーが正しくありません:\n%s", msg)
	}
	// 中国語の件名は RFC 2047 エンコードが必須で、そうしないとクライアントで文字化けする。
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("件名が RFC 2047 でエンコードされていません:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("件名をデコードできません: %v", err)
	} else if !strings.Contains(dec, "SQL注入") {
		t.Fatalf("件名のデコード後の内容が正しくありません: %q", dec)
	}

	// 本文は base64 で、デコード結果は正しい HTML であること。
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("メールにヘッダーと本文の区切りがありません")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("本文の base64 デコードに失敗しました: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("本文が HTML ではありません: %.80s", html)
	}
	// タイトルはテキスト位置にそのまま現れる：HTML テキスト内容の二重引用符は有効な文字なので、エスケープ不要。
	// ここで「そのまま保持」を検証するのは、将来誤って引用符をさらにエスケープして、中国語の引用符が
	// &quot; と表示されるのを防ぐため。
	if !strings.Contains(html, `"SQL注入"`) {
		t.Fatalf("タイトル内の引用符はテキスト位置ではそのまま保持するべきです: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection はメール本文で実際に防ぐ必要があるインジェクションを網羅する：
// 脆弱性のタイトルと要約はテスト対象とモデルの出力に由来する、信頼できない内容。テキスト位置では
// & < > をエスケープする必要があり（タグ注入を防ぐため）、属性位置では引用符もエスケープする（href が閉じられるのを防ぐため）。
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("タイトルがエスケープされておらず、タグの注入が可能です: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("エスケープ済みの実体を期待します: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& と > がエスケープされていません: %s", html)
	}
	// リンク先は管理者が設定できる public_base_url で、それ自体の信頼度は高いが、属性位置では依然として
	// 引用符のエスケープが必要。しないと、引用符を含むアドレスで href が閉じられ、イベントハンドラーが注入される。
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 属性が正しくエスケープされていません: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("属性位置の引用符はエスケープされるべきです: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("%s ヘッダーが見つかりません", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 検証エラーは設定者に直接表示されるため、漠然と「設定が無効」とせず、不足しているものを明示する。
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "ポート"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "宛先"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("チャネル %s が登録されていません", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s の設定 %v は検証に失敗するべきです", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s のエラー情報には %q を含めるべきです。実際の値: %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification は SMTP 4xx/5xx の意味の区別を固定する。
// 4xx まで永続的な失敗と判定すると、グレーリストを有効にしたメールサーバーはどのプッシュも最初の
// 試行後に failed にしてしまう。グレーリストこそ自動再試行が最も役立つ場面である。
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 応答コードが取れない場合は「再試行可能」とする。もう一度試すほうを優先し、一時的な可能性がある
		// 障害を永続的なものと判定しない。
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("收件人被拒", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("応答 %q: 期待値 permanent=%v、実際の値 %v", tc.reply, tc.permanent, got)
		}
		// 分類にかかわらず、調査できるように原文をユーザーに残す。
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("応答 %q の原文が破棄されています: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 6つのチャネルはすべて必須。1つでも欠けると UI のドロップダウンから黙って消えてしまう。
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("チャネル数は %d であるべきです。実際の数: %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("チャネル %s が登録されていません", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("チャネル %s の Kind() が登録キーと一致しません", k)
		}
	}
	if ValidKind("nope") {
		t.Error("未登録の種類は検証に合格するべきではありません")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("恒久的な失敗と認識するべきです")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("エラー情報には下位層の情報をそのまま伝えるべきです: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) は必ず nil を返す必要があります")
	}
	if IsPermanent(nil) {
		t.Fatal("nil は恒久的な失敗ではありません")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
