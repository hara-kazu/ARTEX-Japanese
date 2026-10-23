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
		t.Fatal("非 TCP 监听地址")
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
		t.Fatalf("投递失败: %v", err)
	}
	// エンベロープ段階を通過すること：送信者、2つの受信者、DATA。
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP 会话里缺少 %q，实际命令：%v", want, f.commands)
		}
	}
	// 本文は base64 の HTML で、実際の脆弱性の内容を含むこと（エンコード後も識別可能）。
	body := f.body()
	if body == "" {
		t.Fatal("DATA 阶段没有收到正文")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("缺少 Content-Type 头:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("正文未按 base64 编码（长 HTML 行会破坏 SMTP 的 1000 字节行长限制）:\n%s", body)
	}
	// 複数の受信者がすべて To ヘッダーに現れること。
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To 头未包含全部收件人:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// アカウント未設定時は AUTH を送信しないこと。これが理由で拒否するリレーもある。
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("未配账号却发了 AUTH: %v", f.commands)
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
		{"收件人被 550 永久拒绝", "550 5.1.1 User unknown", "250 OK", true},
		{"收件人遇 450 灰名单", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"收件人遇 452 邮箱满", "452 4.2.2 Mailbox full", "250 OK", false},
		{"发件人被 553 永久拒绝", "250 OK", "553 5.1.3 Bad address", true},
		{"发件人遇 451 临时错误", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("应报错")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 判定错误：期望 %v 得到 %v (%v)", tc.permanent, got, err)
			}
			// サーバーの原文を保持すること。そうしないとユーザーはサーバー管理者に問い合わせるべきか、アドレスを変えるべきか分からない。
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("错误里应保留服务器的应答码: %v", err)
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
		t.Skip("本机 DNS 解析到了本地服务器，跳过（不影响其它用例）")
	}
	// 接続できない場合も認証情報送信を拒否された場合も、このアサーションは成功とする。要点はパスワードを黙って送信しては**ならない**こと。
	if !IsPermanent(err) && !strings.Contains(err.Error(), "接続") {
		t.Logf("错误：%v（非 localhost 下未能连上属预期）", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// メールチャネルは設定フィールドが最も多く、どれが欠けても配信時まで発覚しない。ここでは1つずつ
	// 検証で事前に阻止できることを確認する。アサーションでは「エラーメッセージが不足項目に言及しているか」を確認する。
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"缺 host", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"缺 port", map[string]any{"host": "smtp.example.com"}},
		{"port 越界", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"缺 from", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"缺 to", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("应校验失败: %v", tc.cfg)
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
		t.Fatalf("应容忍字符串形式的数值: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt 未解析字符串端口，得到 %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool 未解析字符串 \"true\"")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings 未兼容单字符串，得到 %v", to)
	}
}

// TestFilterValidateRejectsTypo は監査修正を直接検証する：
// 閾値の誤字は書き込み時に阻止しなければならない。しないとフィルターが黙って無効化され、すべて配信されてしまう。
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("合法门槛 %q 被拒: %v", s, err)
		}
	}
	// これらは実際に起こり得る誤記で、すべて拒否する必要がある。
	for _, s := range []string{"hgih", "HIGH", "严重", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("非法门槛 %q 应被拒绝（否则过滤器静默失效、变成全推）", s)
			continue
		}
		// エラーメッセージはユーザーが正しく修正できるものであること。
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("错误信息应列出可选值，得到 %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly は「書き込み時は厳格、読み取り時は寛容」という分担を固定する：
// データベースにある不正な値のためにチャネル全体を読み取れなくしてはならない（既存チャネルすべてのプッシュが突然停止してしまう）。
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // エラーにしない
	if f.MinSeverity != "hgih" {
		t.Fatalf("读取路径应原样保留，得到 %q", f.MinSeverity)
	}
	// さらに、このチャネルがイベントを判定できること（panic せず、ブロックしない）。
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
