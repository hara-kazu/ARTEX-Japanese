// Package db is the PostgreSQL data source for ARTEX (旧 graph の単一ファイル SQLite を置換)。
// 接続を開き、schema を適用し、組み込み agent と変数カタログを seed する。
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/config"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")
)

//go:embed schema.sql
var schemaSQL string

const schemaMigrationLockKey int64 = 7337741001

// maxOpenConns は接続プールの上限。Open の説明を参照。値は PostgreSQL のデフォルトの
// max_connections=100 より十分低く、アプリ自身の入れ子の接続取得深度（起動時の schema
// advisory lock は1つの接続を保持しながら seedBuiltins に別の接続を取得させる）より十分高いため、自己ロックしない。
const maxOpenConns = 32

var schemaDeadlockRetryDelays = [...]time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isPostgresDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

func applySchemaWithRetry(ctx context.Context, execer schemaExecer, sleep func(time.Duration)) error {
	for attempt := 0; ; attempt++ {
		if _, err := execer.ExecContext(ctx, schemaSQL); err != nil {
			if !isPostgresDeadlock(err) || attempt >= len(schemaDeadlockRetryDelays) {
				return err
			}
			sleep(schemaDeadlockRetryDelays[attempt])
			continue
		}
		return nil
	}
}

// withSchemaMigrationLock pins the session-level lock to one checked-out
// connection. Running pg_advisory_lock through *sql.DB is incorrect because a
// later schema or unlock call may use a different pooled PostgreSQL session.
func withSchemaMigrationLock(ctx context.Context, sqlDB *sql.DB, action func(*sql.Conn) error) (err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaMigrationLockKey); unlockErr != nil && err == nil {
			err = fmt.Errorf("advisory unlock: %w", unlockErr)
		}
	}()
	return action(conn)
}

// coordinateWithSchemaMigration makes long, multi-table archive transactions
// mutually exclusive with startup DDL while allowing ordinary runtime queries
// to continue normally.
func coordinateWithSchemaMigration(tx *sql.Tx) error {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fmt.Errorf("coordinate with schema migration: %w", err)
	}
	return nil
}

// DSN resolves the PostgreSQL connection string and reports where it came from.
// Precedence: env ARTEX_PG_DSN > config file (config.json). There is no
// built-in default — it errors if neither source is configured.
func DSN() (dsn, source string, err error) {
	return config.PostgresDSN()
}

// DB wraps the shared *sql.DB. PG handles its own connection pool + concurrency
// (MVCC), so unlike the old SQLite store there is no process-wide write mutex.
type DB struct{ *sql.DB }

// ensureDatabase connects to the postgres system database and creates the target
// database if it does not exist. dsn must be a postgres:// URL.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // unparseable DSN — let the normal Open fail with a clear error
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || dbName == "postgres" {
		return nil
	}
	// connect to the postgres maintenance database instead
	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		return nil // best-effort; let Open surface the real error
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return nil
	}
	var exists bool
	_ = admin.QueryRow(`SELECT true FROM pg_database WHERE datname=$1`, dbName).Scan(&exists)
	if !exists {
		if _, err := admin.Exec(`CREATE DATABASE "` + dbName + `"`); err != nil {
			return fmt.Errorf("create database %q: %w", dbName, err)
		}
	}
	return nil
}

// Open connects, applies the schema (idempotent), and seeds builtin rows.
func Open(dsn string) (*DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// database/sql はデフォルトで接続数を制限しない：プールに空き接続がなければ無条件で新規接続を作成し、
	// PostgreSQL の max_connections（デフォルト 100）に達して拒否されるため、高負荷時のクエリは
	// `FATAL: sorry, too many clients already` という**エラー**を受け取る。上限を設けると超過したクエリは
	// 空き接続を待つキューに入り、同じ負荷ではエラーから遅延に変わるため、呼び出し元は
	// "読み取れない"と"存在しない"を区別する必要がなくなる。maxOpenConns は psql / reset-password.sh と
	// 共存しうる別インスタンスの余裕を残すこと；max_connections を下げている場合は、ここも下げること。
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", config.Redact(dsn), err)
	}
	d := &DB{sqlDB}
	// pgx runs multi-statement Exec via the simple protocol when there are no args.
	// Keep the dedicated lock connection checked out until both DDL and seeding
	// finish so concurrent application instances cannot initialize out of order.
	err = withSchemaMigrationLock(context.Background(), sqlDB, func(conn *sql.Conn) error {
		if err := applySchemaWithRetry(context.Background(), conn, time.Sleep); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if err := d.seedBuiltins(); err != nil {
			return fmt.Errorf("seed builtins: %w", err)
		}
		return nil
	})
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// builtinAgent describes one of the fixed agents and its prompt-variable catalog.
type builtinAgent struct {
	key, name, role, desc string
	vars                  []promptVar
	interactiveShell      bool // 行作成時のデフォルトの対話型 shell スイッチ；ON CONFLICT はユーザーの後の手動切り替えを上書きしない
	runSeconds            *int // 行作成時の1回の run の実時間上限(秒)；nil=seed のデフォルト(1200)を使用、0=時間制限なし
}

type promptVar struct{ name, desc, example, source string }

// intp は v のポインターを返し、builtinAgent の任意フィールド(runSeconds など)に明示値を与える。
func intp(v int) *int { return &v }

// builtinAgents mirrors docs §5(a). 組み込みツールは DB に保存しない；ここでは agent + 変数カタログのみ seed する。
// 注：planner/worker/mainagent/auto の対話型 shell は、下の interactive_shell_default_v1
// ブロックで一律 true にする（後の toggle を尊重）；ここの interactiveShell は「行作成時からデフォルト有効」が必要な新しい agent 専用。
var builtinAgents = []builtinAgent{
	{"goals", "目標の分解", "goals", "テストの目標を、独立して検証できる複数のサブ目標に分解します。", []promptVar{
		{"EngagementDescription", "タスクの説明（テスト対象 / 背景）", "example.com のサイトをテスト", "exploration"},
		// Now はグローバル runtime 変数(server.globalPromptVars を参照)であり、もはや各 agent のカタログに
		// 重複定義しない。withGlobalVars で追加する際にグローバル項目と名前が重なるため。
	}, false, nil},
	{"planner", "計画", "planner", "状況を確認し、目標の達成を判定します。まだカバーされていない新しい方向が実際にある場合にのみ探索インテントを追加します（タスクごとに1つの計画ループ）。", []promptVar{
		{"Goal", "タスク全体の目標", "example.com の管理者権限を取得", "exploration"},
		{"AssetSummary", "資産数 / 種類別の分布の要約（任意）", "domain:3 ip:5 site:2", "distilled"},
	}, false, nil},
	{"mainagent", "メイン", "main", "人間とシステムのインターフェースとして、進捗を確認し、ユーザーの指示を hint や優先度の高いインテントとして反映します。", []promptVar{
		{"Goal", "現在のタスクの目標", "example.com の管理者権限を取得", "exploration"},
		{"AssetSummary", "開始時の状況の要約（任意）", "domain:3 ip:5", "distilled"},
		{"FindingsSummary", "確認済みの脆弱性の要約（任意）", "high:1 medium:2", "distilled"},
	}, false, nil},
	{"worker", "実行", "worker", "1件のインテントを取得して実行し、検出した事実 / 脆弱性を知識グラフに書き戻して停止します。", []promptVar{
		{"ProxyAddr", "記録プロキシのアドレス（if による2種類の文面の切り替えに使用）", "127.0.0.1:8080", "runtime"},
		{"WorkerName", "worker の識別名（任意）", "worker-1", "runtime"},
	}, false, nil},
	// Auto:組み込みの「プラットフォーム操作」agent。侵入テストのオーケストレーションループには参加せず、対話ページから駆動し、ツールでプラットフォームを操作する。
	{"auto", "Auto", "assistant", "プラットフォーム操作アシスタントです。ツールでタスク（作成 / 表示 / 一時停止 / ヒント）と資産を管理し、skill、カスタムツール、MCP を作成 / 変更します。", nil, false, nil},
	// 侵入テスト:組み込みの「独立侵入テスト」agent。対話ページから駆動し、単独で偵察から終了まで侵入テストの全工程を行い、自ら計画、実行、検証する。対話型 shell はデフォルトで有効。
	{"pentest", "ペネトレーションテスト", "assistant", "独立したペネトレーションテスト agent：一人で偵察→攻撃対象となる箇所の特定→詳細な悪用→検証→終了処理までの全経路を進め、自ら計画・実行・対抗的な検証を行います。", nil, true, intp(0)},
}

// seedBuiltins inserts the fixed built-in agents and their variable catalog (idempotent).
func (d *DB) seedBuiltins() error {
	for _, a := range builtinAgents {
		var agentID int64
		err := d.QueryRow(`
INSERT INTO agents(key, name, description, role, builtin, enabled, interactive_shell, run_seconds)
VALUES ($1, $2, NULLIF($3,''), $4, true, true, $5, COALESCE($6, 1200))
ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description
RETURNING id`, a.key, a.name, a.desc, a.role, a.interactiveShell, a.runSeconds).Scan(&agentID)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.key, err)
		}
		for _, v := range a.vars {
			if _, err := d.Exec(`
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, var_name) DO UPDATE
  SET description = EXCLUDED.description, example = EXCLUDED.example, source = EXCLUDED.source`,
				agentID, v.name, v.desc, v.example, v.source); err != nil {
				return fmt.Errorf("agent %s var %s: %w", a.key, v.name, err)
			}
		}
	}
	// Drop catalog entries for variables that were renamed, so the white-list no
	// longer advertises a name templates can't resolve (EngagementTitle→Description).
	// 'Now' を各 agent のカタログからグローバル runtime 変数に昇格した後、旧 DB の goals に残る 'Now' は
	// グローバル項目と名前が重なる(フロントエンドの変数一覧の key が重複)ため、一緒に削除する。
	if _, err := d.Exec(`DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`); err != nil {
		return fmt.Errorf("cleanup renamed vars: %w", err)
	}
	// Default-on interactive_shell for the runtime agents (planner/worker/mainagent/auto)
	// ONCE — respects a later user toggle-off (guarded by a settings flag). goals(one-shot
	// decomposer) stays off. Runs after the column exists (schema applied before seed).
	if v, _, _ := d.GetSetting("interactive_shell_default_v1"); v != "true" {
		if _, err := d.Exec(`UPDATE agents SET interactive_shell=true WHERE key IN ('planner','worker','mainagent','auto')`); err != nil {
			return fmt.Errorf("seed interactive_shell defaults: %w", err)
		}
		_ = d.SetSetting("interactive_shell_default_v1", "true")
	}
	// Seed the built-in browser (Playwright) MCP once — DISABLED by default (ユーザーが
	// 必要時に自分で有効化), no proxy by default. The traffic-capture toggle injects/strips
	// the recording proxy + CA at runtime (server.Manager.syncBrowserMCPProxy).
	// Insert only if absent so we never clobber user edits (args/env/enabled/
	// visibility) on restart.
	if _, err := d.Exec(`
INSERT INTO mcp_servers(name, transport, command, args, env, enabled)
VALUES ('browser', 'stdio', 'npx', $1, '{}', false)
ON CONFLICT (name) DO NOTHING`,
		`["@playwright/mcp","--headless"]`); err != nil {
		return fmt.Errorf("seed browser mcp: %w", err)
	}
	// NOTE: the placeholder ScopeSentry data-source MCP (empty URL + empty X-API-Key,
	// disabled) is seeded directly in schema.sql §F so a raw `psql < schema.sql` init
	// also gets it. schema.sql is Exec'd on every startup, so it stays idempotent.
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		return fmt.Errorf("seed skill visibility: %w", err)
	}
	if err := d.seedDefaultInterceptRules(); err != nil {
		return fmt.Errorf("seed intercept rules: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV2(); err != nil {
		return fmt.Errorf("seed intercept rules v2: %w", err)
	}
	if err := d.seedDefaultInterceptRulesV3(); err != nil {
		return fmt.Errorf("seed intercept rules v3: %w", err)
	}
	if err := d.seedDefaultAssetInterceptRules(); err != nil {
		return fmt.Errorf("seed asset intercept rules: %w", err)
	}
	return nil
}

// seedDefaultAssetInterceptRules inserts the built-in asset blocklist (fuzzy
// domain matches for government / education sites) once on first startup. Gated
// by a settings flag so a user's later disable/delete is never resurrected on
// restart — same policy as the intercept-rule seed.
func (d *DB) seedDefaultAssetInterceptRules() error {
	if v, _, _ := d.GetSetting("asset_intercept_default_rules_v1"); v == "done" {
		return nil
	}
	rules := []struct {
		kind    string
		pattern string
		note    string
	}{
		{"fuzzy_domain", ".gov", "[組み込み] 政府機関のサイト (.gov)"},
		{"fuzzy_domain", ".gov.cn", "[組み込み] 政府機関のサイト (.gov.cn)"},
		{"fuzzy_domain", ".edu", "[組み込み] 教育機関のサイト (.edu)"},
		{"fuzzy_domain", ".edu.cn", "[組み込み] 教育機関のサイト (.edu.cn)"},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO asset_intercept_rules(enabled, kind, pattern, note, builtin)
VALUES (true, $1, $2, $3, true)
ON CONFLICT DO NOTHING`, r.kind, r.pattern, r.note); err != nil {
			return fmt.Errorf("asset rule %q: %w", r.pattern, err)
		}
	}
	return d.SetSetting("asset_intercept_default_rules_v1", "done")
}

// builtinSkillVisibility maps a shipped skill's directory name → the built-in
// agent keys that should see it by default. The skill FILES themselves live on the
// filesystem (SkillDir, loaded by norma at runtime); DB only carries this visibility
// binding. Skills omitted here (e.g. playwright-cli, scopesentry) ship invisible by
// default — the user turns them on per-agent when needed. scopesentry additionally
// declares `mcps: ScopeSentry`, which only takes effect once it's made visible and
// that MCP is enabled/configured.
var builtinSkillVisibility = map[string][]string{
	"api-recon": {"auto", "pentest", "worker"},
}

// seedBuiltinSkillVisibility binds the shipped built-in skills to their default
// agents. Insert-if-absent (ON CONFLICT DO NOTHING) so a user's later toggle-off is
// never resurrected on restart — matches the browser-MCP / intercept-rule seed policy.
func (d *DB) seedBuiltinSkillVisibility() error {
	for skillName, agentKeys := range builtinSkillVisibility {
		for _, key := range agentKeys {
			if _, err := d.Exec(`
INSERT INTO agent_skill_visibility(agent_id, skill_name, enabled)
SELECT id, $2, true FROM agents WHERE key=$1
ON CONFLICT (agent_id, skill_name) DO NOTHING`, key, skillName); err != nil {
				return fmt.Errorf("skill %s → agent %s: %w", skillName, key, err)
			}
		}
	}
	return nil
}

// seedDefaultInterceptRules inserts built-in safety intercept rules once on
// first startup. The seed is gated by a settings flag so user edits (disable,
// delete, re-order) are never overwritten on subsequent restarts.
func (d *DB) seedDefaultInterceptRules() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v1"); v == "done" {
		return nil
	}
	type rule struct {
		name     string
		target   string // tool_name | tool_input
		typ      string // string | regex
		pattern  string
		action   string
		message  string
		priority int
	}
	rules := []rule{
		// ── システムの破壊的コマンド (priority 100) ──────────────────────────────────
		{
			name:     "[組み込み] 再帰的な強制削除 rm -rf",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\brm\b.{0,80}(?:-[a-z]*r[a-z]*f[a-z]*|-[a-z]*f[a-z]*r[a-z]*|--recursive|--no-preserve-root)`,
			action:   "deny",
			message:  "再帰的な強制削除（rm -rf / rm --recursive）は禁止されています。システムやテスト対象の環境を恒久的に破損するおそれがあります",
			priority: 100,
		},
		{
			name:     "[組み込み] システムの重要ディレクトリの削除",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\brm\b[^"'\n]{0,60}["'\s](/|/etc|/bin|/usr|/boot|/var|/lib|/sys|/proc|/dev|/sbin|/root)`,
			action:   "deny",
			message:  "システムの重要なパスの削除は禁止されています",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクのフォーマット mkfs",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bmkfs\b`,
			action:   "deny",
			message:  "ディスクのフォーマット（mkfs）は禁止されています",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスクデバイスの上書き dd",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bdd\b[^|\n]{0,100}\bof=\s*/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "dd によるディスクデバイスの上書きは禁止されています",
			priority: 100,
		},
		{
			name:     "[組み込み] Fork 爆弾",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `:\(\)\s*\{[^}]*:\|:`,
			action:   "deny",
			message:  "Fork 爆弾の実行は禁止されています",
			priority: 100,
		},
		{
			name:     "[組み込み] シャットダウン / 再起動",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shutdown|reboot|halt|poweroff|init\s+[06])\b`,
			action:   "deny",
			message:  "シャットダウンや再起動のコマンドは禁止されています",
			priority: 100,
		},
		{
			name:     "[組み込み] 全プロセスの強制終了",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\bkill\s+-9\s+-1\b|\bkillall\s+-9\b`,
			action:   "deny",
			message:  "kill -9 -1 または killall -9（全プロセスの強制終了）は禁止されています",
			priority: 100,
		},
		{
			name:     "[組み込み] ディスク消去 shred / wipe",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\b(?:shred|wipe)\b[^|\n]{0,80}/dev/[a-zA-Z]`,
			action:   "deny",
			message:  "ディスクデバイスに対する shred/wipe による消去は禁止されています",
			priority: 100,
		},
		{
			name:     "[組み込み] ファイアウォールルールの全削除",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `\biptables\s+(?:-F|--flush)\b|\bnft\s+flush\s+ruleset\b`,
			action:   "deny",
			message:  "ファイアウォールルールの全削除（iptables -F / nft flush）は禁止されています",
			priority: 100,
		},
		// ── データベースの破壊的操作 (priority 90) ─────────────────────────────────
		{
			name:     "[組み込み] SQL DROP DATABASE / TABLE / SCHEMA",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bDROP\s+(?:DATABASE|TABLE|SCHEMA|INDEX|VIEW|TABLESPACE|USER|ROLE)\b`,
			action:   "deny",
			message:  "DROP 操作は禁止されています。データベースのオブジェクトを復元不能な状態で削除するおそれがあります",
			priority: 90,
		},
		{
			name:     "[組み込み] SQL TRUNCATE",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`,
			action:   "deny",
			message:  "TRUNCATE の実行は禁止されています。テーブル内のすべてのデータを消去するおそれがあります",
			priority: 90,
		},
		{
			name:     "[組み込み] MongoDB drop / dropDatabase",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\.(?:dropDatabase|dropCollection|drop)\s*\(`,
			action:   "deny",
			message:  "MongoDB の drop 操作は禁止されています",
			priority: 90,
		},
		{
			name:     "[組み込み] Redis FLUSHALL / FLUSHDB",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:FLUSHALL|FLUSHDB)\b`,
			action:   "deny",
			message:  "Redis の FLUSHALL / FLUSHDB は禁止されています。すべてのキャッシュデータを消去するおそれがあります",
			priority: 90,
		},
		// ── HTTP の破壊的リクエスト (priority 80) ──────────────────────────────────
		// Agent が DELETE リクエストを送る3つの一般的な方法：
		//   1. curl -X DELETE / --request DELETE（Bash ツールから直接実行、またはスクリプトに記述）
		//   2. Python HTTP クライアントの .delete() メソッド
		//   3. JS/汎用スクリプト内の method: 'DELETE' / method="DELETE"
		{
			name:     "[組み込み] curl / wget による DELETE リクエスト",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\bcurl\b[^|\n&;"]{0,300}(?:-X\s*DELETE|--request\s+DELETE|-XDELETE)|\bwget\b[^|\n&;"]{0,300}--method[=\s]+DELETE`,
			action:   "deny",
			message:  "curl/wget による HTTP DELETE リクエストの送信は禁止されています。対象システムのデータを削除するおそれがあります",
			priority: 80,
		},
		{
			name:     "[組み込み] Python HTTP クライアントの DELETE（requests/httpx/aiohttp）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)\b(?:requests|httpx|aiohttp|urllib\.request)\.delete\s*\(|session\.delete\s*\(|client\.delete\s*\(`,
			action:   "deny",
			message:  "Python HTTP クライアントによる DELETE リクエストの送信は禁止されています",
			priority: 80,
		},
		{
			name:     "[組み込み] スクリプト内の HTTP DELETE メソッド指定（JS / 汎用）",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)axios\.delete\s*\(|method\s*[:=]\s*['"]DELETE['"]`,
			action:   "deny",
			message:  "スクリプト内で HTTP DELETE リクエストを指定して送信することは禁止されています",
			priority: 80,
		},
		{
			name:     "[組み込み] 一括消去 / 削除用インターフェース パス",
			target:   "tool_input",
			typ:      "regex",
			pattern:  `(?i)/(?:clear|wipe|flush|purge|truncate|drop|destroy|factory[-_]reset|reset[-_]all)(?:[/?#"'\s]|$)`,
			action:   "deny",
			message:  "一括消去や破棄を行うインターフェース（/clear /wipe /flush /purge など）の呼び出しは禁止されています",
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, true, $2, $3, $4, $5, $6, $7, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.priority, r.target, r.typ, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v1", "done")
}

// seedDefaultInterceptRulesV2 migrates the two safety patterns that used to be
// hard-coded in guard.go (destructive shell + data-exfil pipe) into ordinary
// intercept rules. Gated by its own flag so it also lands on DBs that already ran
// v1. Unlike the old guard.go floor, these are plain [組み込み] rules — the user can
// disable or delete them. The exfil rule ships DISABLED by default (its
// curl/wget/nc pipe pattern mis-fires on legitimate CTF/pentest reverse-shell and
// data-transfer pipes); enable it manually when exfil gating is actually wanted.
func (d *DB) seedDefaultInterceptRulesV2() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v2"); v == "done" {
		return nil
	}
	rules := []struct {
		name     string
		pattern  string
		action   string
		message  string
		enabled  bool
		priority int
	}{
		{
			name:     "[組み込み] 破壊的なシステムコマンド",
			pattern:  `(?i)\b(rm\s+-rf\s+/|mkfs|dd\s+if=|:\(\)\s*\{|shutdown|reboot|>\s*/dev/sd)`,
			action:   "deny",
			message:  "破壊的なコマンドを拒否しました（rm -rf / / mkfs / dd / fork bomb / シャットダウン・再起動 / ディスクデバイスの上書き）",
			enabled:  true,
			priority: 100,
		},
		{
			name:     "[組み込み] データ持ち出し用パイプ",
			pattern:  `(?i)(curl|wget|nc|ncat)\b[^|]*\b(\|\s*(curl|wget|nc))`,
			action:   "deny",
			message:  "データ持ち出しの疑いがあるパイプを拒否しました（curl/wget/nc によるコマンド出力の外部送信）",
			enabled:  false,
			priority: 80,
		},
	}
	for _, r := range rules {
		if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
VALUES ($1, $2, $3, 'tool_input', 'regex', $4, $5, $6, false, 60, 'deny')
ON CONFLICT DO NOTHING`,
			r.name, r.enabled, r.priority, r.pattern, r.action, r.message,
		); err != nil {
			return fmt.Errorf("rule %q: %w", r.name, err)
		}
	}
	return d.SetSetting("intercept_default_rules_v2", "done")
}

// seedDefaultInterceptRulesV3 adds the delete-endpoint path rule. The v1 HTTP rules
// only catch the DELETE *method* (curl -X DELETE, requests.delete(, method:'DELETE'),
// and v1's path rule covers only /clear /wipe /flush /purge /truncate /drop /destroy
// /factory-reset /reset-all — so a plain `curl 'http://t/api/user/delete?id=1'` (a
// delete endpoint reached with GET/POST, which is how most web apps expose deletion)
// slipped through every built-in rule. Own flag so it also lands on DBs that already
// ran v1/v2, where editing the v1 seed would have no effect.
//
// The pattern deliberately requires a separator after the verb so /delivery,
// /details, /delta and /delegate do not match, while /deleteAll, /delete_user and
// /delete-user do. destroy is re-covered here because v1's rule does not allow a
// suffix (/destroyAll was missed).
//
// Exported as a package const only so the seeded regex is unit-testable without a DB.
const deleteEndpointPathPattern = `(?i)/(?:(?:delete|remove|unlink|erase|destroy)[-\w]*|del)(?:[/?#"'\s]|$)`

func (d *DB) seedDefaultInterceptRulesV3() error {
	if v, _, _ := d.GetSetting("intercept_default_rules_v3"); v == "done" {
		return nil
	}
	const name = "[組み込み] 削除用インターフェース パス"
	if _, err := d.Exec(`
INSERT INTO intercept_rules(name, enabled, priority, match_target, match_type, pattern, action, message, timeout_enabled, timeout_seconds, timeout_action)
SELECT $1, true, 80, 'tool_input', 'regex', $2, 'deny', $3, false, 60, 'deny'
WHERE NOT EXISTS (SELECT 1 FROM intercept_rules WHERE name IN ($1, $4))`,
		name,
		deleteEndpointPathPattern,
		"削除用インターフェース（/delete /remove /unlink /erase など）の呼び出しは、HTTP メソッドに関係なく禁止されています。多くのアプリでは GET/POST でも削除インターフェース を実行でき、対象のデータが実際に削除されます",
		"[内置] 删除类接口路径",
	); err != nil {
		return fmt.Errorf("rule %q: %w", name, err)
	}
	return d.SetSetting("intercept_default_rules_v3", "done")
}
