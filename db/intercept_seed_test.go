package db

import (
	"database/sql"
	"regexp"
	"testing"
)

func TestLocalizedInterceptSeedsPreserveExistingRules(t *testing.T) {
	conn, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	d := &DB{conn}
	if _, err := d.Exec(`CREATE TEMP TABLE settings (key text PRIMARY KEY, value text, updated_at timestamptz DEFAULT now());
CREATE TEMP TABLE intercept_rules (name text, enabled boolean, priority integer, match_target text, match_type text,
pattern text, action text, message text, timeout_enabled boolean, timeout_seconds integer, timeout_action text)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, legacy, localized string
		seed                    func() error
	}{
		{"v3", "[内置] 删除类接口路径", "[組み込み] 削除用 API パス", d.seedDefaultInterceptRulesV3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, existingName := range []string{"", tc.legacy, tc.localized} {
				if _, err := d.Exec(`TRUNCATE settings, intercept_rules`); err != nil {
					t.Fatal(err)
				}
				if existingName != "" {
					if _, err := d.Exec(`INSERT INTO intercept_rules(name,enabled,pattern,action,message) VALUES ($1,false,'custom-pattern','allow','user-edited')`, existingName); err != nil {
						t.Fatal(err)
					}
				}
				if err := tc.seed(); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := d.QueryRow(`SELECT count(*) FROM intercept_rules WHERE name IN ($1,$2)`, tc.legacy, tc.localized).Scan(&count); err != nil || count != 1 {
					t.Fatalf("existing %q: count=%d err=%v", existingName, count, err)
				}
				if existingName != "" {
					var enabled bool
					var pattern, action, message string
					if err := d.QueryRow(`SELECT enabled,pattern,action,message FROM intercept_rules WHERE name=$1`, existingName).Scan(&enabled, &pattern, &action, &message); err != nil {
						t.Fatal(err)
					}
					if enabled || pattern != "custom-pattern" || action != "allow" || message != "user-edited" {
						t.Fatalf("saved rule was changed: enabled=%v pattern=%q action=%q message=%q", enabled, pattern, action, message)
					}
				} else {
					var name string
					if err := d.QueryRow(`SELECT name FROM intercept_rules WHERE name IN ($1,$2)`, tc.legacy, tc.localized).Scan(&name); err != nil {
						t.Fatal(err)
					}
					if name != tc.localized {
						t.Fatalf("new rule used legacy name %q", name)
					}
				}
			}
		})
	}
}

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
