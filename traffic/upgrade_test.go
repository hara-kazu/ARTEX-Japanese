package traffic

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openLegacyIndex builds the index exactly as the pre-reclamation Open did: a
// plain-path DSN, pragmas via the pool, and auto_vacuum left at its default 0.
func openLegacyIndex(t *testing.T, dir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "_index"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(dir, "_index", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := old.Exec(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(indexSchema); err != nil {
		t.Fatal(err)
	}
	return old
}

// TestUpgradeFromOldInstall guards the upgrade path. Open now names the database
// through a file: URI so per-connection pragmas can ride in the DSN, and a
// driver that did not treat that as a URI would quietly open a file literally
// named "file:/…" — an empty index, with every recorded exchange apparently
// gone. The assertions below are what prove that does not happen.
func TestUpgradeFromOldInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_index", "index.sqlite")
	old := openLegacyIndex(t, dir)
	if _, err := old.Exec(ftsSchema); err != nil {
		t.Fatal(err)
	}
	// 過去のトラフィック3件。legacy path<>'' の行を1件含む
	for i, row := range [][]any{
		{"1700000000-0001", "old.example.com", ""},
		{"1700000000-0002", "old.example.com", ""},
		{"1700000000-0003", "legacy.example.com", "legacy.example.com/GET/x"},
	} {
		if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,'GET','/x','http://x/x',200,'text/html',0,9,?)`, row[0], 1700000000+i, row[1], row[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body)
VALUES(?,'GET /x','','HTTP 200','老数据正文')`, row[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, i+1, "老数据正文 secret-token"); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// ---- 新版による引き継ぎ
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("新バージョンで旧データベースを開けません: %v", err)
	}
	defer tr.Close()

	// 1. 同じファイルである必要があり、黙って新しい空のデータベースを開いてはならない
	if st2, err := os.Stat(path); err != nil || st2.Size() == 0 {
		t.Fatalf("元の索引ファイルが不正です: size=%v err=%v", st2, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "_index")); len(entries) > 3 {
		for _, e := range entries {
			t.Logf("_index 内: %s", e.Name())
		}
		t.Fatal("_index 内に想定外のファイルがあり、DSN が別のデータベースを指している可能性があります")
	}
	t.Logf("旧データベースは %d バイトで、新バージョンで引き継いでも同じファイルです", stat.Size())

	// 2. 過去のデータがすべて見える
	n, err := tr.Count()
	if err != nil || n != 3 {
		t.Fatalf("Count=(%d,%v)、期待値 (3,nil) —— 過去のトラフィックが失われています", n, err)
	}
	// 3. 過去の全文インデックスを引き続き検索できる
	if tr.fts {
		rows, err := tr.query("old.example.com", "", "secret-token", 0, 10)
		if err != nil {
			t.Fatalf("過去の全文検索に失敗しました: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("過去の全文検索が %d 件一致しました。期待値 2", len(rows))
		}
	}
	// 4. 過去の本文を引き続き読める
	if _, resp, err := tr.Get("1700000000-0001"); err != nil {
		t.Fatalf("過去の本文の読み取りに失敗しました: %v", err)
	} else if resp == "" {
		t.Fatal("過去のレスポンスが空です")
	}
	// 5. 旧データベースを増分回収が有効と誤判定しない
	if tr.incrementalVacuum {
		t.Fatal("旧データベースで増分回収が有効と誤判定されました")
	}
	// 6. 削除が引き続き正常動作し、旧データベースで回収処理が収束する
	deleted, err := tr.DeleteHostsExact([]string{"old.example.com"})
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteHostsExact=(%d,%v)、期待値 (2,nil)", deleted, err)
	}
	tr.reaping.Wait()
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("削除後の Count=(%d,%v)、期待値 (1,nil)", n, err)
	}
	// 7. legacy path<>'' の行に影響しない
	var legacyPath string
	if err := tr.DB().QueryRow(`SELECT path FROM exchanges`).Scan(&legacyPath); err != nil {
		t.Fatal(err)
	}
	if legacyPath == "" {
		t.Fatal("legacy 行の path が消去されています")
	}
}

// TestDowngradeToOldBinary covers a rollback: a database created with
// auto_vacuum=incremental must stay readable and writable by a build that knows
// nothing about it. auto_vacuum only changes where SQLite tracks free pages, so
// the old binary simply goes back to never returning them.
func TestDowngradeToOldBinary(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.incrementalVacuum {
		t.Fatal("新データベースでは増分回収を有効にすべきです")
	}
	bulkRecord(tr, "keep.example.com", 5, 100*1024)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	old := openLegacyIndex(t, dir) // 旧版のバイナリーによる引き継ぎ
	defer old.Close()
	var n int
	if err := old.QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("旧バージョンで読み取った値 (%d,%v)、期待値 (5,nil)", n, err)
	}
	if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('x',1,'new.example.com','GET','/x','http://x/x',200,'',0,0,'')`); err != nil {
		t.Fatalf("旧バージョンでの書き込みに失敗しました: %v", err)
	}
	if _, err := old.Exec(`DELETE FROM exchanges WHERE host='keep.example.com'`); err != nil {
		t.Fatalf("旧バージョンでの削除に失敗しました: %v", err)
	}
}
