package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// このファイルは doJSON の HTTP 層のエラー分類を検証する。
//
// 個別にテストする理由：各チャネルアダプターはプラットフォーム独自の業務エラーコード（DingTalk の errcode、
// Feishu の code、Telegram の ok フィールド）のみ扱い、**HTTP 層**の分類は doJSON が統一して行う。
// 両者は独立した2つの防御であり、これがないと 503 を返す中継ゲートウェイを永続的な失敗として
// 直ちに再試行を断念し、403 を再試行可能として無駄に3回バックオフしてしまう。

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 の成功はエラーとしない", 200, false},
		{"429 のレート制限は再試行可能", 429, false},
		{"408 のリクエストタイムアウトは再試行可能", 408, false},
		{"500 のサーバーエラーは再試行可能", 500, false},
		{"502 のゲートウェイエラーは再試行可能", 502, false},
		{"503 のサービス利用不可は再試行可能", 503, false},
		{"400 の引数エラーは恒久的失敗", 400, true},
		{"401 の認証エラーは恒久的失敗", 401, true},
		{"403 のアクセス禁止は恒久的失敗", 403, true},
		{"404 のアドレス不存在は恒久的失敗", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx はエラーを返すべきではありません: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx 以外はエラーを返すべきです")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d の permanent 判定が正しくありません: 期待値 %v、実際の値 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// エラーにステータスコードが現れる必要がある。ないとユーザーは自分の誤設定か相手の障害か判断できない。
			// Go の英語の StatusText ではなく数値を検証する：本パッケージの文言は中国語であり、
			// （プロジェクトの他の部分と同じ）、数値こそが言語に依存せず安定して検証できる部分である。
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("エラー情報には HTTP 状態コード %d を含めるべきです。実際の値: %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet は snippet を検証する：相手が返したエラー説明を持ち帰る必要がある。
// そうしないと、ユーザーは「失敗した」ことしか分からず、相手が拒否した理由が分からない。
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーを返すべきです")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("エラー情報には接続先の説明を含めるべきです。実際の値: %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded は snippet の形を制約する：
// 相手の応答はそのまま last_error 列とフロントエンドの表に入り、複数行や長すぎる内容はレイアウトとペイロードを壊す。
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 改行、タブ、5000 文字の長すぎる内容を含む応答。
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーを返すべきです")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("エラー情報は 1 行にまとめるべきです。実際の値: %q", msg)
	}
	// snippet の上限は 200 文字 + 固定の接頭辞で、全体は元の応答よりはるかに小さくなければならない。
	if len(msg) > 400 {
		t.Errorf("エラー情報が長すぎます（%d バイト）。snippet で切り詰めるべきです: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse は読み取りに上限があることを確認する：相手が異常に巨大な内容を返しても
// 応答全体をメモリーに読み込んではならない（配信履歴では各項目に last_error が保存される）。
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーを返すべきです")
	}
	if len(err.Error()) > 400 {
		t.Errorf("過大な応答は読み取りサイズを制限して切り詰めるべきです。エラー情報の長さ: %d", len(err.Error()))
	}
}
