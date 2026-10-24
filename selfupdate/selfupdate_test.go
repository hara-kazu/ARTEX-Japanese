package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths は隔離した更新ディレクトリーを作る。ResolvePaths() を直接使ってはならない。テスト
// バイナリー自身を指し、実行すると go test の実行ファイルをリネームしてしまうため。
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin は実行可能なシェルスクリプトを書き、artex を装う。smokeTest は -h で起動して終了コードを見るだけなので、
// スクリプトで十分であり、実際のバイナリーをコンパイルするよりはるかに速い。
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("模擬バイナリ %s の書き込み: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage は bin を「一時保存済み、置換待ち」の状態にする：artex.new とそのチェックサムを書き込む。
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("チェックサムの計算: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("チェックサムの書き込み: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s の読み取り: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("模擬バイナリは sh スクリプトのため、Windows 上では実行できません")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh は v を除去し、tag には v が付く。両方を認識する必要がある
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 辞書順ではなく数値で比較する
		{"1.0.0", "0.99.99", 1, true},
		// 開発ビルドは比較不能と判定する必要があり、しないと正式版が未コミットの変更を上書きしてしまう。
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v, 期待値 %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d, 期待値 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 重要な不変条件：すべての更新ファイルは実行ファイルと同じディレクトリーに置く。CWD に置くと、サービス稼働時
	// （作業ディレクトリーは / の可能性がある）の置換が完全に無効になる。
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s が実行ファイルのディレクトリ内にありません: %s (期待値 %s)", name, path, p.Dir)
		}
	}
	// Windows では .new/.old に .exe を残す必要があり、なければスモークテストと置換後の実行がどちらも失敗する。
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows では .new/.old は .exe で終わる必要があります: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// チェックサムを書いた後にファイルを変更し、ダウンロードの破損 / すり替えを模擬する。
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 の不一致を拒否するはずが、通過しました")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 実行できるが終了コードが 0 でない

	if err := verifyStaged(p); err == nil {
		t.Fatal("スモークテストの失敗を拒否するはずが、通過しました")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("マーカーの書き込み: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart を期待しましたが、%v でした", action)
	}
	if !st.Pending {
		t.Error("入れ替え後の状態は Pending であるべきです")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex は新しいバージョンに置き換わっているべきです")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("旧バージョンは artex.old にバックアップされているべきです")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("入れ替え後は artex.new が存在しないべきです")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("入れ替え後はチェックサムファイルが削除されているべきです")
	}
	// マーカーを残す必要がある。次回起動（新版が動く）でカウントし、必要ならロールバックするため。
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("入れ替え後も更新マーカーは保持されているべきです")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // チェックサムを破損させる

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("検証失敗時は Continue を期待しましたが、%v でした", action)
	}
	if !st.FailedStage {
		t.Error("状態は FailedStage と記録すべきです")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("検証失敗時は現在のバージョンを絶対に変更してはいけません")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("検証に失敗した一時ファイルは削除すべきです。そうでなければ次の起動時に再試行されます")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 前回更新で残ったバックアップ
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3 に入れ替わっているべきです")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("バックアップは直前に入れ替えた v2 に更新されているべきです")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// 最初の maxAttempts 回の起動ではカウントのみ加算し、新版に安定動作の機会を与える。
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d 回目の試行で Continue を期待しましたが、%v でした", i, action)
		}
		if !st.Pending {
			t.Errorf("%d 回目の試行の状態は Pending であるべきです", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d 回目の試行後は attempts=%d（ok=%v）でした。期待値 %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// もう1回クラッシュすると上限を超え、自動で旧版に戻す。
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("試行上限を超えた場合は Restart を期待しましたが、%v でした", action)
	}
	if !st.RolledBack {
		t.Error("状態は RolledBack と記録すべきです")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("旧バージョンにロールバック済みであるべきです")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("ロールバック後はマーカーを削除すべきです。そうでなければロールバックを無限に繰り返します")
	}
	// 起動できないバージョンは調査用に残し、直接削除しない。
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("失敗したバージョンは調査用に .failed として保持すべきです")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() は ResolvePaths() を通すため、ここでは下位の交換の意味を直接テストする。
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("ロールバック後の現在のバージョンは v1 であるべきです")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("ロールバック後のバックアップは v2 に変わり、再度戻せるべきです")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum 出力は二重の空白区切り。shasum -a 256 はバイナリーモードでファイル名に * を付ける。
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // ちょうど2フィールドだが、最初のフィールドはダイジェストでない
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // ダイジェストの長さが不正

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux エントリーの解析エラー: %v", out)
	}
	// ダイジェストは小文字に統一し、照合時に大文字小文字によって不一致と誤判定しないようにする。
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows エントリーのエラー（* の接頭辞を除去し、ダイジェストを小文字にするべきです）: %q", got)
	}
	if len(out) != 2 {
		t.Errorf("空行、ダイジェスト以外の行、長さが不正な行は無視すべきですが、%v でした", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("パッケージ内のベース名は Windows では artex.exe ですが、このケースは Unix の名前で構成しています")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 実際の公開パッケージの構造：artex-<バージョン>-<os>-<arch>/artex に加え、いくつかのダミーファイル。
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("展開したファイルが artex の実行ファイルではありません: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("展開したバイナリには実行属性が必要です")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("パッケージに実行ファイルがない場合はエラーにすべきです")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS 以外
		"https://evil.com/artex.zip",    // ドメインが許可リストにない
		"https://github.com.evil.com/x", // 接尾辞の偽装
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) は拒否すべきです", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // ドメインの大文字小文字を区別しない
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) は許可すべきですが、エラーになりました: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh の package_binary は artex-<バージョン>-<os>-<arch>.zip を使い、バージョン番号の
	// v 接頭辞を除去する。ここで1文字でも違うと、すべてのプラットフォームのワンクリック更新でアセットが見つからなくなる。
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q の解析: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("安定動作の確認後は更新マーカーを削除する必要があります")
	}
	// マーカーが消えたため、その後の通常の再起動では回数を加算せず、誤ってロールバックも発生しない。
	if _, ok := readMarker(p.Marker); ok {
		t.Error("マーカーの読み取りは失敗すべきです")
	}
	// バックアップは残し、ユーザーが手動でロールバックできるようにする。
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("安定動作の確認後も直前のバージョンのバックアップは保持すべきです")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 通常の起動経路。panic もファイルの変更も起こさないこと
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("マーカーがない場合、settle はどのファイルにも影響すべきではありません")
	}
}
