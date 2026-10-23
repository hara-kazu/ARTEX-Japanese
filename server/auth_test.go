package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting の「キーなし」と「読み取りエラー」の戻り値は error だけが異なり、どちらも value は
// 空文字列です。パスワード関連の handler が error を「まだ未設定」と扱うと、DB 障害時に
// 初期化を許可してしまいます。authInit は未認証の要求による既存の管理者パスワードの上書きを許し、
// authStatus はその操作を行う /setup へフロントエンドを直接送ってしまいます。
//
// この二つのテストは handler の接続プールを閉じて読み取り失敗を起こし、両方が fail closed になることを検証します。
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// プールを閉じ、以後の GetSetting が sql.ErrNoRows ではなく error を返すようにします。
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (読み取り失敗を未初期化と扱うと、ユーザーを /setup に誘導してパスワードを上書きします); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("読み取り失敗時は initialized と応答すべきではありませんが、%v でした", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (読み取り失敗時に許可すると、未認証のリクエストが既存パスワードを上書きします); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("読み取り失敗時は token を発行すべきではありません: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"空", "", true},
		{"7文字", "1234567", true},
		{"8文字", "12345678", false},
		{"漢字8文字はバイト数ではなく文字数で数える", "密码密码密码密码", false},
		{"漢字3文字は9バイトでも3文字しかない", "密码强", true},
		{"72バイト", strings.Repeat("a", 72), false},
		{"73バイトは bcrypt の上限を超える", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
