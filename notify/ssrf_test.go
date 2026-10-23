package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// このファイルは関連する2つの強化を検証する：
//   ① 配信先アドレスがサーバーを踏み台に内部ネットワーク / クラウドメタデータへアクセスしてはならない（SSRF）
//   ② アドレス検証のエラーメッセージに、アドレス内の認証情報を含めてはならない
//
// テスト環境について：本パッケージの多くのケースは 127.0.0.1 の httptest のダミー受信側を使い、ガードはデフォルトで
// それらを阻止する。そのため TestMain で AllowLocalTargetsEnv をまとめて有効にし、以下の各 SSRF ケースでは
// 明示的にクリアして、**デフォルトで拒否**の動作を検証する。

func TestMain(m *testing.M) {
	// 通常のケースがローカルのダミー受信側へ接続できるようにする。SSRF ケースでは自ら一時的にクリアする。
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault は SSRF 防御の中心となるアサーション：
// デフォルト設定では、ループバックアドレスへの配信を**接続層**で拒否する必要がある。
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 例外の入口を無効化 = デフォルト動作
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("默认不应允许投递到环回地址")
	}
	if hit {
		t.Fatal("请求已经打到了本机服务——守卫没生效")
	}
	// エラーメッセージは許可方法をユーザーに案内できること（ローカル SMTP リレーは正当な設定）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒绝信息应说明如何显式放开: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn は逆方向のケース：明示的に有効にしたら使用できる必要がある。
// そうしないと、ローカルの postfix / 内部ネットワークのリレーなどの正当なデプロイを一律に使えなくしてしまう。
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("显式放开后应可投递: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // クラウドメタデータのエンドポイント。本関数が存在する主な理由
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped 形式は元に戻してから判定する必要があり、そうしないと回避の入口になる
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拒绝", s)
		}
	}
	// RFC1918 のプライベートネットワークは**意図的に許可**する：内部ネットワークの自前 Mattermost / SMTP リレーは一般的な正当な用途。
	// このアサーションはその判断を固定し、将来誰かがついでにプライベートネットワーク判定を追加すると失敗することで、
	// 意識した判断を促す（デプロイ群を黙って使えなくしないため）。
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 应被放行（私网是常见的合法投递目标）", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets は設定段階での事前通知を検証する：
// IP リテラルは初回配信の失敗を待つのではなく、保存時に拒否する必要がある。
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s 应在配置阶段被拒绝", raw)
		}
	}
	// 公開アドレスとプライベートアドレスは通常どおり通す（プライベートネットワークはダイヤル段階に任せ、そこでは阻止しない）。
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s 应通过校验: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials は監査で指摘された前回の漏れの分岐。
//
// url.Parse が**失敗**すると *url.Error を返し、Error() に完全な元のアドレスが含まれる。前回は
// http.Client.Do の戻りエラーのみ機密情報を除去し、ここを漏らしていた。当時追加した「永続的な失敗の経路」のケース
// （file://、gopher://、ftp://）は実際にはすべて url.Parse で解析できて scheme 分岐を通るため、
// 全件成功でもこの経路の安全性を証明できず、偽りの保証だった。
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 不正なパーセントエスケープ
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // ポートが数値でない
		"http://[::1?access_token=" + leakProbeToken,                  // 括弧が対応していない
	}
	for _, raw := range cases {
		// 先にこの入力で**実際に** url.Parse が失敗することを確認する。これをしないと、ケースが
		// 気付かないまま別の分岐へ進む可能性がある（前回の偽りの保証はこうして生じた）。
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q 本应解析失败，否则这条用例没有覆盖到目标分支", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q 应校验失败", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// チャネル層のラッパーもアドレスを持ち出していないことを確認する。
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("非法地址应校验失败")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault は SMTP チャネルのダイヤルガードを検証する。
//
// メールチャネルは以前、素の net.Dialer を使っており、SSRF 防御全体で唯一の抜け穴だった：host に
// 169.254.169.254 や 127.0.0.1 を指定すれば直接接続でき、smtp.NewClient のハンドシェイク失敗時に
// 相手が返した行をエラーに含め、last_error 経由で配信履歴 API に返していた。まさに他のチャネルが
// すでに閉じた半ブラインド読み取りのプリミティブであり、「接続拒否 vs タイムアウト」の時間差でポート探索もできた。
//
// 本パッケージの TestMain は AllowLocalTargetsEnv を全体で有効にする（多くのケースが 127.0.0.1 の
// ダミー受信側を使う）ため、このケースでは自らクリアする必要がある。そうしないとガードの有無にかかわらず成功し、
// これこそ当初どのテストでも抜け穴が見つからなかった理由である。
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 例外の入口を無効化 = デフォルト動作
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("默认不应允许把邮件投递到环回地址")
	}
	// 接続自体が確立されないこと。ガードが Control フックで阻止し、EHLO が送信されることはない。
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 会话已经建立——守卫没生效")
	}
	// エラーメッセージは許可方法をユーザーに案内できること（ローカルの postfix リレーは正当な設定）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒绝信息应说明如何显式放开: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn は対になる逆方向のケース：明示的に有効にしたら
// 正常に配信できる必要がある。内部ネットワークの自前 SMTP / ローカルリレーは非常に一般的なデプロイで、ガードが一律拒否してはならない。
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("显式放开后本机 SMTP 应可投递: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("未看到 EHLO——会话没真正建立")
	}
}
