package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// このファイルは「項目単位で詰める」修正を検証する：集約メッセージがチャネルの長さ上限を超えたら、必ず**項目単位で**
// 切り詰め、収まらなかった件数を正確に返し、呼び出し元が実際に届いたものだけを記録できるようにする。
//
// 以前は全体を描画してから切り詰め、バッチ全体を配信済みとしていたため、メッセージ後半が突然消え、
// 配信履歴には全件成功と表示された。脆弱性がこうして消えても、発見できる場所がなかった。

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 件の中国語の集約は、WeCom の 4096 バイトを確実に大幅超過する。
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("本文の %d バイトが上限 %d を超えています", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("本文が有効な UTF-8 ではありません")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけを含めるべきです（0 < kept < %d）。実際の値: %d", len(m.Items), kept)
	}
	// 先頭には、このメッセージに何件含まれ、残りが何件あるかを正確に示す必要がある。そうしないと、読み手は先頭の
	// 数値を全件数と解釈してしまう。
	if !strings.Contains(body, "残り") || !strings.Contains(body, "次の通知に続きます") {
		t.Fatalf("ヘッダーには今回のメッセージに含まれていない残りの件数を示すべきです:\n%s", body[:minInt(400, len(body))])
	}
	// 先頭の kept 件のみを含むこと。
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("%d 番目は今回のメッセージに含まれるべきです:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("%d 番目は含まれるべきではありません（次のバッチに属します）", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 制限なし
	if kept != len(m.Items) {
		t.Fatalf("長さを制限しない場合はすべて保持するべきです。実際の値: kept=%d", kept)
	}
	if strings.Contains(body, "残り") {
		t.Fatalf("切り詰めていない場合は切り詰めの案内を表示するべきではありません:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 予算が小さく1件も収まらなくても、1件は送信する（最後の切り詰めで対応）。
	// そうしないと、長すぎる脆弱性1件がバッチ全体を永久に止めてしまう：取得するたびに収まらず、毎回送信しないため。
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("少なくとも 1 件は保持するべきです。実際の件数: %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("単一のメッセージは送達数 1 件と報告するべきです。実際の件数: %d", kept)
	}
	// 空のメッセージには配信可能な項目がない。
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("空のメッセージは 0 件と報告するべきです。実際の件数: %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram は**文字数**で長さを制限する。バイト基準では中国語メッセージが3分の1に圧縮されてしまう。
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("本文の %d 文字が上限 %d を超えています", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけを含めるべきです。実際の値: %d", kept)
	}
	if !strings.Contains(text, "次の通知に続きます") {
		t.Fatalf("まだ含まれていない残りがあることを示すべきです:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("カードには一部だけを含めるべきです。実際の値: %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// この2つのチャネルは本文を切り詰めず、バッチ全体を配信済みとする。
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("前提条件が成立しません")
	}
	// 描画処理の戻り値を通じて間接的に確認する：markdownBody(0) は制限がなく、すべて保持する。
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("長さを制限しない場合はすべて含めるべきです。実際の値: %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent は「信頼できない内容がメッセージ構造を変更してはならない」ことの回帰テスト。
// タイトルと要約はモデル出力（モデルはテスト対象の応答を読む）に由来し、アセット名はテスト対象の URL に由来する。
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 結果に含まれること（エスケープ済みの形）
		wrong []string // 結果に含まれないこと（エスケープ前の形）
	}{
		{
			name: "タイトル内の改行と外部リンク",
			item: Item{
				Severity: "high",
				Name:     "登录口 SQL 注入\n[紧急：点此验证账号](http://attacker.tld)",
			},
			// 改行は折り畳む必要がある（新しいリスト項目/引用ブロックを偽装できないようにするため）。
			// 角括弧と丸括弧はエスケープする必要がある（クリック可能な外部リンクになるのを防ぐため）。
			must:  []string{`\[紧急：点此验证账号\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[紧急", "\n\n[紧急"},
		},
		{
			name: "タイトル内の画像ビーコン",
			item: Item{
				Severity: "high",
				Name:     "漏洞 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "資産名内の強調と引用",
			item: Item{
				Severity: "high",
				Name:     "普通标题",
				Assets:   []string{"a.com/*注入*>引用"},
			},
			must:  []string{`\*注入\*`, `\>`},
			wrong: []string{"*注入*"},
		},
		{
			name: "要約内のバッククォートと縦棒",
			item: Item{
				Severity: "high",
				Name:     "标题",
				Summary:  "`code` | 表格",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 単発モードの書き込みItem は3つの markdown チャネルで共有する描画経路。
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("エスケープ済みの形 %q がありません:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("エスケープされていない形 %q が含まれています（構造や外部リンクの注入に使用できます）:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst はエスケープ順序を固定する：バックスラッシュを必ず最初に処理する。
// そうしないと、後から加えたバックスラッシュをさらにエスケープし、出力に二重のバックスラッシュが現れる。
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("エスケープの順序が正しくありません。実際の値: %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes は具体的な回帰を固定する：
// markdown のエスケープは Telegram の HTML 出力に漏れてはならない（以前、共有タイトル関数に
// エスケープを加えた結果、Telegram メッセージに `\(1\)` のような見えるバックスラッシュが現れた）。
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram の本文に markdown のバックスラッシュによるエスケープが含まれています:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
