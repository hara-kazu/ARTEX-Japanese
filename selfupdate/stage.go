package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset は release.yml が生成するチェックサム一覧で、Release 内のすべての zip をカバーする。
const sumsAsset = "SHA256SUMS"

// maxBinarySize は解凍したバイナリーのサイズを制限し、不正な zip によるディスクの使い切りを防ぐ。
const maxBinarySize = 512 << 20 // 512 MiB

// Phase は更新過程の段階で、SSE イベントの phase フィールドに直接使う。
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress は呼び出し元が提供し、進捗をフロントエンドにプッシュするために使う。pct はダウンロード段階のみ意味があり（0-100）、
// それ以外の段階では -1 を渡す。
type Progress func(ph Phase, pct int, msg string)

// Stage は指定した Release の現在のプラットフォーム用の公開パッケージをダウンロードし、検証後に新バイナリーを artex.new として一時保存する。
//
// 素のバイナリーではなく完全な zip を使う理由は2つ。既存の Release の SHA256SUMS はもともと
// zip のみをカバーするため、zip を使えば CI の変更が不要で、公開済みの過去のバージョンにも対応できる。また zip には
// skills/ も含まれ、将来の組み込み skill 同期の余地がある。代償は skills の数百 KB を余分にダウンロードするだけ。
//
// 関数から戻れば一時保存完了で、呼び出し元は続いて正常にシャットダウンし、ExitRestart で終了する。
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("このバージョンには %s/%s 向けの配布パッケージがありません（不足：%s）", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "チェックサム一覧を取得しています…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s に %s が含まれていません。検証できない実行ファイルのインストールを拒否しました", sumsAsset, name)
	}

	// 一時ファイルはすべて配置先ディレクトリーに置き、最後の rename が同じファイルシステム内のアトミックな操作であることを保証する
	// （デバイスをまたぐ rename は失敗し、/tmp は独立したマウントポイントであることが多い）。
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s（%s）をダウンロードしています…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 を検証しています…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 が一致しません。期待値は %s、実際は %s です（ダウンロード時の破損または改ざん）", short(want), short(got))
	}

	prog(PhaseExtract, -1, "展開して動作を確認しています…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("新しいバージョンを現在のシステムで実行できません：%w", err)
	}

	// 一時保存ファイル自身の sha256 は別に保存する。次回起動の置換前にも再度検証し、
	// 一時保存後から再起動前までの間のファイルの変更や書き込み破損を防ぐため。
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("新しい実行ファイルのチェックサム計算に失敗しました：%w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("チェックサムの書き込みに失敗しました：%w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("新しいバージョンの一時保存に失敗しました：%w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// マーカーは自動ロールバック機能にのみ影響する。一時保存ファイル自体は配置済みなので、それを理由に更新を中断しない。
		prog(PhaseStaged, -1, "警告：更新マーカーを書き込めませんでした。今回の更新では自動ロールバックを利用できません")
	}

	prog(PhaseStaged, 100, "新しいバージョンの準備ができました。再起動しています…")
	return nil
}

// fetchSums は SHA256SUMS をダウンロードして解析し、ファイル名 → 16進数のダイジェストを返す。
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("この Release に %s がないため、整合性を検証できません。更新を拒否しました", sumsAsset)
	}
	body, err := get(ctx, c, asset.downloadURL())
	if err != nil {
		return nil, fmt.Errorf("%s のダウンロードに失敗しました：%w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s の読み取りに失敗しました：%w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s の内容が空か、形式を認識できません", sumsAsset)
	}
	return out, nil
}

// parseSums は sha256sum 形式の一覧を解析し、ファイル名 → 16進数のダイジェストを返す。
//
// 最初のフィールドが 64 桁の16進数の場合のみ収録する。「ちょうど2フィールド」だけの判定では不十分で、
// 任意の2単語の説明行を正当な項目とみなし、ダイジェスト表にごみの値を入れてしまい、
// 実際のアセットが誤ったダイジェストと照合される可能性がある。
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 形式は "<sha256>  <filename>"（sha256sum は二重の空白、shasum のバイナリー
		// モードではファイル名に * 接頭辞を付ける）。
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download はアセットを dst に書き込み、同時に SHA256 を計算して Content-Length に基づく進捗を報告する。
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.downloadURL())
	if err != nil {
		return "", fmt.Errorf("%s のダウンロードに失敗しました：%w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("一時ファイルの作成に失敗しました：%w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("ダウンロードが中断しました：%w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("ディスクへの書き込みに失敗しました：%w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("ダウンロードが不完全です。期待値は %d バイト、実際は %d バイトです", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get は許可リストに制限された GET を実行し、応答ボディを返す。
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	req.Header.Set("Accept", "application/octet-stream")
	setUpdateAuth(req)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary は公開パッケージから artex 実行ファイルを取り出す。
//
// パッケージ内の構造は artex-<バージョン>-<os>-<arch>/artex だが、ここでは完全なパスを組み立てず、**ベース名**で照合する。
// バージョン番号はパッケージ名内にすでに1回現れており、1文字間違えると更新全体が失敗するため、ベース名で探すほうが変更に強い。
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("配布パッケージを開けませんでした：%w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s の読み取りに失敗しました：%w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("新しい実行ファイルの書き込みに失敗しました：%w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s の展開に失敗しました：%w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("配布パッケージ内の実行ファイルが %s を超えるため、展開を拒否しました", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("配布パッケージ内の %s は空のファイルです", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("配布パッケージに %s が見つかりません", want)
}

// checkWritable はディレクトリーへの書き込み可否を事前に確認する。これがないと、root 以外での実行時やバイナリーがシステム
// ディレクトリーにある場合、数十 MB のダウンロード完了後、置換の瞬間になって初めて失敗する。
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("プログラムのディレクトリ %s に書き込めないため、自動更新できません（権限を確認するか、手動で更新してください）：%w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter は書き込み済みバイトを集計して報告頻度を制限し、32KiB のブロックごとに SSE をプッシュするのを避ける。
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("ダウンロード中 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
