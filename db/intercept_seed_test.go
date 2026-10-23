package db

import (
	"regexp"
	"testing"
)

// 組み込みの「削除系 API パス」ルールは tool_input の JSON 文字列全体を照合するため、ケースも直接
// JSON 形式で与え、Interceptor が実際に受け取る subject と一致させる。
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET で削除 API を呼び出す
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST で削除 API を呼び出す
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 のパスルールは接尾辞を許可しないので、ここで追加する
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("应命中却放行: %s", s)
		}
	}

	// 動詞の後に区切り文字を必須とし、/delivery、/details のような読み取り専用パスの誤遮断を防ぐ。
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 削除動詞がパスではなくドメインに出現する
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("误拦: %s", s)
		}
	}
}

func TestInterceptSeedPreservesLegacyRule(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	d.SetMaxOpenConns(1)
	d.SetMaxIdleConns(1)
	// Keep fixtures on this connection without changing shared seed rows or flags.
	for _, query := range []string{
		`CREATE TEMP TABLE settings (LIKE public.settings INCLUDING ALL)`,
		`CREATE TEMP TABLE intercept_rules (LIKE public.intercept_rules INCLUDING ALL)`,
	} {
		if _, err := d.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"", "[内置] 删除类接口路径", "[組み込み] 削除用インターフェース パス"} {
		t.Run(name, func(t *testing.T) {
			if _, err := d.Exec(`TRUNCATE settings, intercept_rules`); err != nil {
				t.Fatal(err)
			}
			if name != "" {
				if _, err := d.Exec(`INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message)
VALUES ($1, false, 17, 'tool_input', 'regex', 'custom-pattern', 'ask', 'custom-message')`, name); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if _, err := d.Exec(`DELETE FROM settings WHERE key='intercept_default_rules_v3'`); err != nil {
					t.Fatal(err)
				}
				if err := d.seedDefaultInterceptRulesV3(); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := d.QueryRow(`SELECT count(*) FROM intercept_rules`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("rule count=%d err=%v", count, err)
			}
			var actualName, pattern, action, message string
			var enabled bool
			var priority int
			if err := d.QueryRow(`SELECT name, enabled, priority, pattern, action, message FROM intercept_rules`).Scan(
				&actualName, &enabled, &priority, &pattern, &action, &message); err != nil {
				t.Fatal(err)
			}
			if name != "" {
				if actualName != name || enabled || priority != 17 || pattern != "custom-pattern" || action != "ask" || message != "custom-message" {
					t.Fatalf("existing rule changed: %q %v %d %q %q %q", actualName, enabled, priority, pattern, action, message)
				}
			} else if actualName != "[組み込み] 削除用インターフェース パス" || !enabled || action != "deny" || pattern != deleteEndpointPathPattern {
				t.Fatalf("new rule differs: %q %v %q %q", actualName, enabled, action, pattern)
			}
		})
	}
}
