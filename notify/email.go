package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout はそれぞれ接続確立と SMTP セッション全体を制限する。
// net/smtp 自体にはタイムアウト機構がなく、この2つを設定しないと、停止した相手が
// 配信 goroutine を永久に待機させる。dispatcher は単一の goroutine で逐次処理するため、
// 通知システム全体の停止に相当する。
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel は SMTP メール配信を実装する。
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// メールにはプラットフォームのレート制限がないが、大量送信を避けるため緩めのデフォルト値を設ける。
func (emailChannel) DefaultRatePerMin() int { return 60 }

// パスワードのみマスクする。SMTP ホスト、アカウント、宛先は秘密情報ではなく、マスクすると編集が面倒になるだけ。
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port はパスワードを渡すサーバーを決め、tls は暗号化通信の有無を決める。この3つのいずれかが変わったら
// パスワードの再明示を要求する。これにより「TLS を無効にする」際も、ついでの変更では済まず、認証情報を明示する必要がある。
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP サーバーのアドレスがありません")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP ポートが無効です（1～65535）")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("差出人のアドレスがありません")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("宛先のアドレスを1つ以上指定してください")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS：相手がサポートしていればアップグレードする。平文セッションでは認証情報を送れない（下の auth の説明を参照）。
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS に失敗しました: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth は暗号化されていない接続での認証情報送信を拒否する（送信先が localhost の場合を除く）。
			// これは**正しい**セキュリティ動作であり、回避してはならないが、理由を明確に訳す必要がある。
			// そうしないとユーザーには「unencrypted connection」しか表示されず、対処法が分からない。
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("接続が暗号化されていないため、認証情報を送信しません。TLS を有効にするか、ポート465（暗黙的 TLS）を使用するか、「TLS を有効にする」を選択してください (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 認証に失敗しました: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("差出人 %s が拒否されました", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("宛先 %s が拒否されました", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA に失敗しました: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("メール本文の書き込みに失敗しました: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("メールの送信に失敗しました: %w", err)
	}
	// Quit が失敗しても「メールがサーバーに受信された」という事実に影響しないため、そのエラーは無視する。
	_ = client.Quit()
	// メールは長さで切り詰めず（HTML 本文をすべて送信）、バッチ全体を配信済みとする。
	return len(m.Items), nil
}

// emailDial は SMTP 接続を確立する。
//
// implicitTLS=true は 465 のような「接続直後から TLS」の方式、false は 25/587 で平文接続を確立した後に
// STARTTLS を行う方式。混同してはならず、465 ポートに平文の greeting を送ると即切断される。
//
// セッション期限は**接続確立時**に設定する（後付けしない）。net/smtp の Client は下位の
// 接続を非公開フィールドに隠していて外部から取得できないため、接続を渡した後は事前設定した deadline
// に頼るしかない。これによりハンドシェイク段階の待機もカバーする。
// Control に blockInternalDial を設定し、HTTP 系チャネルと同じガードを共有する。設定しないと SMTP
// が SSRF 防御全体の抜け穴になる：host に 169.254.169.254 や 127.0.0.1 を指定すれば直接接続でき、
// smtp.NewClient のハンドシェイクが失敗すると相手が返した行がエラーに含まれ、last_error 経由で
// 配信履歴 API に返されるため、半ブラインド読み取りのプリミティブになる。「接続拒否 vs タイムアウト」の時間差も
// ポート探索に使えてしまう。ダイヤル段階が最終的な適用箇所で、DNS リバインディングにも対応する。
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP サーバーに接続できません: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP ハンドシェイクに失敗しました: %w", err)
	}
	return client, nil
}

// smtpStageError は SMTP 応答コードに従い、ある段階の失敗を「再試行可能」と「永続的な失敗」に分類する。
//
// 区別が必要な理由：SMTP の 4xx と 5xx は意味がまったく異なる。
//   - 4xx（450 グレーリスト、451 ローカルエラー、452 ストレージ不足）は**一時的な**拒否で、
//     後で再試行するのが正しい。特にグレーリストは、ほぼすべての初回配信で遭遇する。
//   - 5xx（550 ユーザー不存在、553 不正なアドレス）は永続的な拒否であり、再試行は無意味。
//
// すべて永続的な失敗と判定すると、グレーリストを有効にしたメールサーバーは**すべての**プッシュを最初の
// 試行後に failed にしてしまう。この種の失敗こそ自動再試行が最も役立つ場面である。
// 応答コードはエラーテキストの先頭3桁から取得する。取得できなければ再試行可能とする（もう一度試すほうを優先し、
// 解析できないだけで一時的な可能性のある障害を永続的なものと判定しない）。
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode は SMTP エラーテキストの先頭3桁の応答コードを取得し、取れなければ 0 を返す。
// net/smtp はエラーコードのフィールドを公開しないため、テキストから取得するしかない。形式は「450 4.7.1 ...」。
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage は完全な RFC 5322 メールを組み立てる。
//
// 本文を base64 エンコードする理由は2つ。1つ目は SMTP が1行 1000 バイト以下と定めており、HTML
// 本文（特に集約メール）では長すぎる行が生じやすいため。2つ目は base64 では "." で始まる
// 行が生じないため、SMTP のドットエスケープの手間を省ける。
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 中国語の件名は RFC 2047 エンコードが必須で、そうしないとクライアントで文字化けする。
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// メールには長さの厳密な上限がないため、本文を切り詰めない。
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 は 76 文字で折り返し、RFC 2045 に準拠する。
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
