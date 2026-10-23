package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// このファイルは不変条件のテスト：チャネル実装から発生する**すべての**エラーテキストに、認証情報が含まれてはならない。
//
// 個別のファイルにした理由：当初のチャネルのケースは成功経路とプラットフォームの業務エラーのみを検証し、
// 通信層の失敗はまったく確認していなかった。しかし通信層のエラー（接続拒否/DNS 失敗/タイムアウト）こそ最も危険で、
// http.Client.Do が返す *url.Error はエラーテキストに**完全な URL**を入れるが、この機能の
// 各サービスの認証情報は URL 内にある。認証情報はこの文字列を通り、4つの出口に流れた：
//
//	notification_deliveries.last_error  → 平文でデータベース保存
//	GET /api/notify/deliveries の応答     → チャネル設定のマスクを回避し、ブラウザーに返す
//	サーバーログ                          → 外部送信して保存されることが多い
//	テスト送信 API の 502 応答             → フロントエンドに直接表示
//
// そのため、ここでは関数1つだけでなく、チャネルごとに必ず失敗するリクエストを実際に1回送信し、エラーテキストに
// その認証情報が見つからないことを検証する。

// credentialCases は「認証情報が URL 内にある」チャネルの全形態を網羅する：
// DingTalk/WeCom は query、Feishu はパス末尾部分、Telegram はパス途中の部分。
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk の access_token が query 内にある",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom の key が query 内にある",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu の hook id がパスの末尾にある",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram の bot token がパスの途中にある",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk の署名キー",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken は実際の認証情報では絶対にあり得ない番兵値で、エラーテキスト内で検索するために使う。
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials は中心となる不変条件。
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 必ず失敗する相手：127.0.0.1:1 はリスナーがなく、接続拒否の経路を通る。
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "泄露探针"}},
			})
			if err == nil {
				t.Fatal("到達できないアドレスではエラーを返すべきです")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath は永続的な失敗の分岐を検証する：
// URL 検証失敗やプラットフォームの業務エラーなどもエラーテキストを外部に渡すため、同様に認証情報を含めてはならない。
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 認証情報を含むが形式が不正なアドレス → validateHTTPURL / url.Parse 分岐を発生させる。
		{"DingTalk のアドレスが無効", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"WeCom のアドレスが無効", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Feishu のアドレスが無効", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API のアドレスが無効", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"汎用 Webhook のアドレスが無効", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("不正な設定ではエラーを返すべきです")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("エラーテキストが認証情報 %q を漏えいしています:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q、期待値 %q", in, got, want)
		}
		// 機密情報除去後の結果自体に、元のアドレスのパス/クエリーの断片が一切含まれないこと。
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("秘匿化後もパス/クエリの断片 %q が含まれています: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 解析不能な入力では元の文字列を決して返さない。
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("解析できない入力 %q が %q として返されています", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL は *url.Error という具体的な型を直接確認する：
// http.Client.Do が返す型であり、漏洩が最初に発生する場所でもある。
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("調査のために host を保持するべきです。実際の値: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("調査のために下位層の原因を保持するべきです。実際の値: %q", got)
	}
	// Op も保持する必要がある（POST か GET かは調査に役立つ）。
	if !strings.Contains(got, "Post") {
		t.Errorf("操作名を保持するべきです。実際の値: %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback はフォールバック経路：*url.Error 以外のカスタムエラー
// （リダイレクトポリシーが返すエラーなど）内のアドレスも同様に除去する必要がある。
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("拒绝跨主机重定向（a.example → http://b.example/bot%s/send）", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("アドレスを秘匿化した形に置換するべきです。実際の値: %q", got)
	}
	// アドレスを含まないテキストはそのまま保持する。
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("アドレスを含まないテキストは変更するべきではありません")
	}
}

// TestCrossHostRedirectRefused は「認証情報が URL 内にある + ホストをまたぐ遷移に追従 = 認証情報を渡す」を検証する。
// httptest の2つのサービスは 127.0.0.1 の異なるポートで待ち受け、ポートが異なれば Host も異なるため、
// ちょうどホストをまたぐ遷移になる。
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("異なるホストへのリダイレクトは拒否されるべきです")
	}
	if hit {
		t.Fatal("リダイレクト先にアクセスしました——認証情報がリダイレクトとともに外部へ漏えいしています")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed は逆方向のケース：同じホスト内の遷移（末尾にスラッシュを付ける場合など）は引き続き利用できる必要がある。
// そうしないと通常のワークフローまで阻止してしまう。
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 同じホスト、同じポートへの遷移。
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("同じホストへのリダイレクトは拒否されるべきではありません: %v", err)
	}
}
