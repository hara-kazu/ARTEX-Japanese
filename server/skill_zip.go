package server

import (
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Go の archive/zip の組み込み展開器は Store(0) と Deflate(8) の2種類のみで、別の方式では
// "zip: unsupported compression algorithm" を返す。圧縮ソフトはデフォルト以外の設定で別方式をよく使用するため、
// (7-Zip の bzip2、WinZip の zstd)、純粋な Go で解ける2種類をここで追加する；本当に展開できないものは
// (Deflate64 / LZMA / XZ / PPMd / 暗号化アーカイブ)は展開前に日本語で説明し、下位の
// エラーをそのままユーザーへ投げない。
const (
	zipMethodStore     = 0
	zipMethodDeflate   = 8
	zipMethodDeflate64 = 9
	zipMethodBzip2     = 12
	zipMethodLZMA      = 14
	zipMethodZstdPKW   = 20 // PKWARE が初期に zstd へ割り当てた番号
	zipMethodZstd      = 93
	zipMethodXZ        = 95
	zipMethodJPEG      = 96
	zipMethodWavPack   = 97
	zipMethodPPMd      = 98
	zipMethodAES       = 99
)

var zipMethodNames = map[uint16]string{
	zipMethodStore:     "Store",
	zipMethodDeflate:   "Deflate",
	zipMethodDeflate64: "Deflate64",
	zipMethodBzip2:     "bzip2",
	zipMethodLZMA:      "LZMA",
	zipMethodZstdPKW:   "Zstandard",
	zipMethodZstd:      "Zstandard",
	zipMethodXZ:        "XZ",
	zipMethodJPEG:      "JPEG",
	zipMethodWavPack:   "WavPack",
	zipMethodPPMd:      "PPMd",
	zipMethodAES:       "AES 暗号化",
}

func zipMethodName(m uint16) string {
	if n, ok := zipMethodNames[m]; ok {
		return n
	}
	return "不明"
}

// newSkillZipReader parses an uploaded archive and registers the extra decompressors
// we can support beyond the stdlib's Store/Deflate.
func newSkillZipReader(buf []byte) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("アーカイブを解析できません（zip 形式が必要）：%w", err)
	}
	zr.RegisterDecompressor(zipMethodBzip2, func(r io.Reader) io.ReadCloser {
		return io.NopCloser(bzip2.NewReader(r))
	})
	zdec := zstd.ZipDecompressor(zstd.WithDecoderConcurrency(1))
	zr.RegisterDecompressor(zipMethodZstd, zdec)
	zr.RegisterDecompressor(zipMethodZstdPKW, zdec)
	return zr, nil
}

// skillZipEntry pairs a zip entry with its decoded (UTF-8) name — f.Name may hold
// raw GBK bytes, see zipEntryName.
type skillZipEntry struct {
	f    *zip.File
	name string
}

// skillZipEntries lists the archive's real files (no directory entries, no archiver
// junk) with their names decoded to UTF-8.
func skillZipEntries(zr *zip.Reader) []skillZipEntry {
	out := make([]skillZipEntry, 0, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := zipEntryName(f)
		if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") ||
			path.Base(name) == ".DS_Store" {
			continue // macOS の梱包残骸
		}
		out = append(out, skillZipEntry{f: f, name: name})
	}
	return out
}

// zipEntryName returns the entry path as UTF-8. Windows の 7-Zip / WinRAR / エクスプローラーは
// UTF-8 フラグを立てない場合、中国語ファイル名を GBK で zip に書き込み、Go はバイトをそのまま保持するため、名前は
// 有効な UTF-8 でなく、パス検証も通らない —— ここで GBK による復号にフォールバックする。
func zipEntryName(f *zip.File) string {
	if utf8.ValidString(f.Name) {
		return f.Name
	}
	if dec, err := simplifiedchinese.GBK.NewDecoder().String(f.Name); err == nil && utf8.ValidString(dec) {
		return dec
	}
	return f.Name
}

// checkSkillZipMethods rejects archives we cannot extract, naming the offending
// entry and method instead of letting f.Open() fail with an opaque English error.
func checkSkillZipMethods(entries []skillZipEntry) error {
	for _, e := range entries {
		if e.f.Flags&0x1 != 0 || e.f.Method == zipMethodAES {
			return fmt.Errorf("アーカイブが暗号化されています（%s）。暗号化されていない zip を使用してください", e.name)
		}
		switch e.f.Method {
		case zipMethodStore, zipMethodDeflate, zipMethodBzip2, zipMethodZstd, zipMethodZstdPKW:
		default:
			return fmt.Errorf("未対応の圧縮方式 %s（method %d）：%s。"+
				"「無圧縮」または「Deflate」で再圧縮してください(7-Zip/WinRAR では圧縮方式に Deflate を選択するか、"+
				"システム標準の「圧縮/圧縮フォルダーに送る」、またはコマンドラインの zip -r を使用してください)",
				zipMethodName(e.f.Method), e.f.Method, e.name)
		}
	}
	return nil
}
