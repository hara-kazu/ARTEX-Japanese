package notify

import (
	"net/url"
	"testing"
	"time"
)

// 署名の基準値は OpenSSL で独立して計算し、本パッケージ自身の実装では生成しない。
// そうしないと「コードが不変」しか証明できず、「アルゴリズムが正しい」とは証明できないため。
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("署名に失敗しました: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("出力したアドレスを解析できません: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("署名が一致しません\n期待値 %s\n実際の値 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("タイムスタンプはミリ秒で、そのまま含めるべきですが、%q でした", q.Get("timestamp"))
	}
	// 既存の query パラメーター（access_token）を署名で上書きしてはならない。
	if q.Get("access_token") != "tok" {
		t.Errorf("元の query パラメーターが失われています。結果は %q でした", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("署名が一致しません\n期待値 %s\n実際の値 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer は両サービスのアルゴリズムの違いを固定する。互いに引数の順序がちょうど逆であり、
// （DingTalk は key=secret、Feishu は key=署名対象文字列）、他方を写すと必ず検証に失敗する。
// このケースは将来のリファクタリングで両者を同じ関数に統合しないよう保証する。
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk と Feishu の署名が同じで、どちらかのアルゴリズムの実装が誤っています")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 署名を有効にしていないボット：timestamp/sign パラメーターを勝手に追加してはならない。
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret が未設定ならアドレスを変更すべきではありませんが、%q でした", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q は受け付けるべきです: %v", s, err)
		}
	}
	// file:// などを許可してはならない。http.Client の処理が想定範囲を超えるため。
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q は拒否すべきです", s)
		}
	}
}
