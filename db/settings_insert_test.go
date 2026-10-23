package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent は auth.password_hash の最終防御：呼び出し元の GetSetting 確認は
// DB エラーで機能しない場合や、並行リクエストに割り込まれる場合がある（bcrypt は数十ミリ秒かかる）ため、"初回のみ
// 設定可能"の保証は上位の if 判定ではなく主キー制約に委ねること。
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// テスト専用 key を使用し、開発 DB の実際の auth.password_hash には触れない。
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("首次写入应返回 inserted=true")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("键已存在时应返回 inserted=false")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("值被覆盖成 %q，应保持 %q", got, "first")
	}
}
