package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// これは本パッケージで最も重要な不変条件。WeCom は**バイト**で長さを制限し、中国語は1文字 3 バイトなので、
	// バイトで強制的に切る実装は漢字を途中で切り、不正な UTF-8 を生成してプラットフォームに拒否される。
	// 長さが互いに素となる複数の中国語と英語の混在入力で、可能なすべての切断位置を試す。
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4 バイトの emoji では切り間違いがより明白
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("入力 %q max=%d: 無効な UTF-8 %q が生成されました", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("入力 %q max=%d: 結果の %d バイトが上限を超えています", in, max, len(got))
			}
			// 切り詰めない場合は内容を変更してはならない。
			if len(in) <= max && got != in {
				t.Fatalf("入力 %q max=%d: 上限を超えていないのに内容が変更されました -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 は制限なしを意味するべきです")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 は制限なしを意味するべきです")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max が省略記号自体より小さい場合、省略記号の追加で逆に超過してはならない。
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 の場合、結果 %q の長さ %d が上限を超えています", got, len(got))
	}
	// 通常は省略記号を付けること。
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("省略記号を含むことを期待しましたが、実際の値は %q です", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes との基準の差を保持する必要がある：Telegram は文字で長さを制限し、
	// バイト基準だと中国語メッセージが3分の1に切り詰められる。
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("5 文字を期待しましたが、実際は %d 文字です (%q)", n, got)
	}
	// 同じ文字列をバイト基準で切ると明確に短くなること。
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("バイト単位では文字単位と同じ文字数を生成するべきではありません")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("第一行\n\n第二行\t带制表   多空格", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("すべての空白を折りたたむべきです。実際の値: %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("連続するスペースを保持するべきではありません。実際の値: %q", got)
	}
	// 切り詰め後も読めて有効であること。
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("4文字を期待しましたが、%d (%q) でした", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML を直接切り詰めると `<a href="htt` のような断片となり、プラットフォームがメッセージ全体を拒否する。
	s := `<b>标题</b>正文正文正文<a href="https://example.com/very/long/path">查看详情</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 結果の %d 文字が上限を超えています", max, n)
		}
		// 末尾に閉じていない `<` がないこと（最後の部分に `<` があるのに `>` がない状態）。
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 末尾のタグが途中で切れています -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("資産がない場合は空文字列を返すべきですが、%q でした", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a、b" {
		t.Fatalf("上限以内ならすべて列挙すべきですが、%q でした", got)
	}
	// 上限超過時は総数を示す必要があり、そうしないと読み手は未列挙のアセット数が分からない。
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "ほか、合計5件") {
		t.Fatalf("総数 5 を明記すべきですが、%q でした", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("空の重大度の順序値は 0 であり、どの閾値でも拒否すべきです")
	}
	if !AtLeast("critical", "") {
		t.Fatal("空の閾値では許可すべきです")
	}
	if got := StatusLabel("fixed"); got != "修正済み" {
		t.Fatalf("不明な状態の対応付けです。結果は %q でした", got)
	}
	// 不明な状態はそのまま返し、ラベルを推測して作らない。
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("不明な状態はそのまま表示すべきですが、%q でした", got)
	}
}

// TestTruncateHTMLNeverCutsEntity は監査で指摘された漏れを検証する：切り詰めではタグの
// 途中だけでなく、切れた HTML 実体参照も避ける必要がある。
//
// `&amp;` が `&amp` に切られると、実体参照のみ認識する解析器でメッセージ**全体**が拒否される可能性がある。
// 長すぎる集約メッセージはもともとよくあるため、損失が大きすぎる。
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 末尾に「& があるのに対応する ; がない」実体参照の断片が現れないこと。
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 末尾にエンティティの断片 %q が残っています", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 不正なエンティティが発生しています", max)
		}
	}
}
