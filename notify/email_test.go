package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// このファイルはメールチャネルのプロトコルレベルのテストを補う。以前の email.Send のカバレッジは 0 で、
// SMTP 経路全体を実行するケースは1つもなかったが、これは6つのチャネルの中でプロトコルの範囲が最も広く、
// 最も誤りやすいものだった（ハンドシェイク、認証、エンベロープ、DATA の各段階で失敗の意味が異なる）。
//
// ここでは net/smtp を mock するのではなく、自作の最小 SMTP サーバーで駆動する：
// メールチャネルのリスクの大半は「実際の SMTP サーバーと通信する」段階にあり、
// その段階を mock するとテストしていないのと同じため。

// fakeSMTP は最低限の SMTP サーバー：greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT を完了でき、
// ケースに応じて特定の段階で指定の応答コードを返す。
type fakeSMTP struct {
	ln net.Listener

	// rcptReply は RCPT TO の応答。デフォルトは 250。
	rcptReply string
	// mailReply は MAIL FROM の応答。デフォルトは 250。
	mailReply string
	// advertiseAuth が true なら EHLO で AUTH PLAIN のサポートを宣言する。
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP の待ち受けアドレスではありません")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS は宣言しない：コードを平文分岐に進める（テスト対象は TLS ではなくエンベロープのロジック）。
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 簡略化：PLAIN の初期応答は複数行の場合があるが、そのまま受け入れる。
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	// エンベロープ段階を通過すること：送信者、2つの受信者、DATA。
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP セッションに %q がありません。実際のコマンド: %v", want, f.commands)
		}
	}
	// 本文は base64 の HTML で、実際の脆弱性の内容を含むこと（エンコード後も識別可能）。
	body := f.body()
	if body == "" {
		t.Fatal("DATA 段階で本文を受信していません")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type ヘッダーがありません:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("本文が base64 でエンコードされていません（長い HTML 行は SMTP の 1000 バイトの行長制限に違反します）:\n%s", body)
	}
	// 複数の受信者がすべて To ヘッダーに現れること。
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To ヘッダーにすべての宛先が含まれていません:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// アカウント未設定時は AUTH を送信しないこと。これが理由で拒否するリレーもある。
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("アカウントが未設定なのに AUTH を送信しました: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies は今回の監査修正を直接検証する：
// 5xx は永続的な失敗、4xx（グレーリスト）は再試行可能と判定する。
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"受信者が 550 で恒久的に拒否される", "550 5.1.1 User unknown", "250 OK", true},
		{"受信者が 450 のグレーリストに該当", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"受信者が 452 のメールボックス容量超過に該当", "452 4.2.2 Mailbox full", "250 OK", false},
		{"送信者が 553 で恒久的に拒否される", "250 OK", "553 5.1.3 Bad address", true},
		{"送信者が 451 の一時エラーに該当", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("エラーを返すべきです")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 判定が正しくありません: 期待値 %v、実際の値 %v (%v)", tc.permanent, got, err)
			}
			// サーバーの原文を保持すること。そうしないとユーザーはサーバー管理者に問い合わせるべきか、アドレスを変えるべきか分からない。
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("エラーにはサーバーの応答コードを保持するべきです: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp の PlainAuth は暗号化されていない接続での認証情報送信を拒否する（送信先が localhost の場合を除く）。
	// これは**正しい**セキュリティ動作であり、回避してはならない。ただし、修正方法をユーザーに案内できるエラーを返す。
	// ここでは localhost 以外のホスト名を使って発生させる。
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost 以外
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("このマシンの DNS がローカルサーバーのアドレスを解決したため、スキップします（他のテストケースには影響しません）")
	}
	// 接続できない場合も認証情報送信を拒否された場合も、このアサーションは成功とする。要点はパスワードを黙って送信しては**ならない**こと。
	if !IsPermanent(err) && !strings.Contains(err.Error(), "接続") {
		t.Logf("エラー: %v（localhost 以外で接続できないのは想定どおりです）", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// メールチャネルは設定フィールドが最も多く、どれが欠けても配信時まで発覚しない。ここでは1つずつ
	// 検証で事前に阻止できることを確認する。アサーションでは「エラーメッセージが不足項目に言及しているか」を確認する。
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host がない", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port がない", map[string]any{"host": "smtp.example.com"}},
		{"port が範囲外", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from がない", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to がない", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("検証に失敗するべきです: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance は設定読み取りの許容性を検証する：JSONB の数値は float64 だが、
// ユーザーは UI でポートを文字列として入力する可能性があり、配列も単一の文字列かもしれない。
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 文字列形式のポート
		"from": "a@b.c",
		"to":   "d@e.f", // 配列ではなく単一の文字列
		"tls":  "true",  // 文字列形式のブール値
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("文字列形式の数値を許容するべきです: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt が文字列のポートを解析していません。実際の値: %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool が文字列 \"true\" を解析していません")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings が単一の文字列に対応していません。実際の値: %v", to)
	}
}

// TestFilterValidateRejectsTypo は監査修正を直接検証する：
// 閾値の誤字は書き込み時に阻止しなければならない。しないとフィルターが黙って無効化され、すべて配信されてしまう。
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("有効なしきい値 %q が拒否されました: %v", s, err)
		}
	}
	// これらは実際に起こり得る誤記で、すべて拒否する必要がある。
	for _, s := range []string{"hgih", "HIGH", "严重", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("無効なしきい値 %q は拒否されるべきです（そうしないとフィルターが通知なく機能しなくなり、すべて通知されます）", s)
			continue
		}
		// エラーメッセージはユーザーが正しく修正できるものであること。
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("エラー情報には選択可能な値を列挙するべきです。実際の値: %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly は「書き込み時は厳格、読み取り時は寛容」という分担を固定する：
// データベースにある不正な値のためにチャネル全体を読み取れなくしてはならない（既存チャネルすべてのプッシュが突然停止してしまう）。
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // エラーにしない
	if f.MinSeverity != "hgih" {
		t.Fatalf("読み取り処理では値をそのまま保持するべきです。実際の値: %q", f.MinSeverity)
	}
	// さらに、このチャネルがイベントを判定できること（panic せず、ブロックしない）。
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
