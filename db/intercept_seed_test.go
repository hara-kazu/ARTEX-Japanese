package db

import (
	"regexp"
	"testing"
)

// 内置「删除类接口路径」规则匹配的是整个 tool_input JSON 串，因此用例直接以
// JSON 形态给出，与 Interceptor 实际拿到的 subject 一致。
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET 打删除接口
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST 打删除接口
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 的路径规则不允许后缀，这里补上
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("应命中却放行: %s", s)
		}
	}

	// 动词后必须跟分隔符，避免 /delivery、/details 这类只读路径被误拦。
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 删除动词出现在域名而非路径
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
