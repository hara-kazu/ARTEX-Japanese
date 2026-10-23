// Package selfupdate implements ARTEX の画面でのワンクリック更新：GitHub Release から新版の
// バイナリーを取得、検証、一時保存し、次回起動時にアトミックに置き換える。
//
// 全体の役割分担（start.sh / start.bat を参照）：
//
//	起動スクリプト  = 単純な監視ループで、「プロセス終了後、終了コードで再起動の要否を判断」のみ担当
//	本パッケージ    = 間違えやすい全ロジック（ダウンロード / SHA256 検証 / スモークテスト / 置換 / 失敗時のロールバック）
//
// 置換をスクリプトではなく Go に置くのは、sha256 検証とスモークテストを sh と bat で
// 2組書く必要があり（sha256sum / shasum / certutil）、こここそ最も間違いを許せない段階だから。実行できない
// バイナリーに置き換えると、監視プロセスが忠実に再起動を繰り返し、ユーザーはマシン上で手作業で復旧するしかなくなる。
//
// 1回の完全な更新は、3回のプロセス起動を通る：
//
//	① 旧版の server が /api/update/apply を受信 → ダウンロードと検証 → artex.new を一時保存 → exit 75
//	② スクリプトが旧版を再起動 → Bootstrap が artex.new を検出 → 検証+スモークテスト → 置換 → exit 75
//	③ スクリプトが再起動し、今度は新版 → Bootstrap が試行を1回記録 → 起動成功後にマーカーを消去
//
// どの段階で失敗しても旧版に戻る：② 検証失敗なら一時保存ファイルを削除して旧版を続行。③ 連続 3 回、
// マーカー消去まで生き延びない（起動できずクラッシュする）場合は artex.old に自動で戻す。
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart は「監視プロセスに再起動を依頼する」終了コード（EX_TEMPFAIL）。起動スクリプトはこれを見たら
// 即再実行し、クラッシュのバックオフには数えない。0 はユーザーによる正常停止（スクリプトはループを終了）、それ以外はすべてクラッシュとみなす。
const ExitRestart = 75

// maxAttempts は置換後に許す起動試行回数。新版は起動のたびにカウントを +1 し、
// settleDelay を生き延びたらマーカーを消去する。maxAttempts 回連続クラッシュは新版が起動できないことを示し、自動でロールバックする。
const maxAttempts = 3

// Paths は1回の更新に関わる全ファイルで、すべて**実行ファイルの所在ディレクトリー**に置く。
// CWD を意図的に使わない：サービス稼働時の作業ディレクトリーは / や任意のパスになり得るため、CWD を使うと一時保存ファイルが
// 別の場所に置かれ、置換ロジックが直接無効になる。
type Paths struct {
	Dir     string // 実行ファイルの所在ディレクトリー
	Current string // 現在実行中のバイナリー        artex      / artex.exe
	New     string // 一時保存した新版              artex.new  / artex.new.exe
	Sum     string // 新版の sha256（hex）          artex.new.sha256 / artex.new.exe.sha256
	Old     string // 置換前にバックアップした旧版  artex.old  / artex.old.exe
	Marker  string // 更新状態マーカー              artex.upgrade.json
}

// ResolvePaths は現在の実行ファイルから更新パス全体を導出する。
//
// Windows では .new/.old にも .exe 接尾辞が必須で、なければスモークテストと置換後の実行がどちらも失敗するため、
// 先に接尾辞を外してから組み立て、両プラットフォームの命名を対称にする。
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("実行ファイルの特定：%w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows では ".exe"、Unix では通常空
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker は置換の進捗を記録し、新版が起動できないときに自動ロールバックを発生させるために使う。
type marker struct {
	From     string `json:"from"`     // 更新前のバージョン
	To       string `json:"to"`       // 対象のバージョン
	Attempts int    `json:"attempts"` // 置換後に起動を試行した回数
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged は一時保存ファイルを消去する。置換成功、検証失敗、ユーザーのキャンセルではすべてこれを通し、残った
// artex.new が次回起動時に再試行されるのを防ぐ。
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions は2つのバージョン番号を比較し、-1/0/1（a<b / a==b / a>b）を返す。
// ok=false は少なくとも片方が比較可能なバージョン番号でないことを示す（ローカル開発ビルドの "dev" や
// git describe が生成する "0.3.7-2-gabc1234-dirty" など）。この場合、呼び出し元はワンクリック更新を無効にする必要がある。
// そうしないと開発中のビルドが正式版に「更新」され、未コミットの変更が上書きされてしまう。
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion は "v0.3.7" / "0.3.7" 形式のバージョン番号を [3]int に解析する。
//
// 純粋な3部分形式のみ受け入れる：build.sh は tag 以外のビルドで git describe を使って
// "0.3.7-2-gabc1234" のような接尾辞付きバージョンを生成する。これらは比較不能と判定する必要があり、
// 0.3.7 とみなしてはならない。そうしないと、開発ビルドが「すでに最新」と誤判定されたり、正式版で上書きされたりする。
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker はプロセスがコンテナー内で動作しているかを返す。Docker 内の置換はコンテナーの書き込み可能層に書くため、
// `docker compose up -d` でコンテナーを再作成すると、イメージ付属のバージョンに戻る。これは期待する動作である
// （その時点ではユーザーも新しいイメージを取得している）が、フロントエンドはこれに応じて明確に説明する必要がある。
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
