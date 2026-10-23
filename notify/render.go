package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes は s を max バイト以下に切り詰め、結果が有効な UTF-8 で、文字を分断しないことを保証する。
//
// 文字境界で切る必要がある理由：WeCom グループボットの markdown は 4096 **バイト**の厳密な上限を持ち（
// 文字数ではない）、中国語1文字は 3 バイト。バイト単位で直接スライスすると漢字を途中で切り、不正な
// UTF-8 を生成するため、プラットフォームがメッセージ全体を拒否するか、文字化けした四角を表示する。ここでは予算の位置から
// 直前の rune の開始バイトまで戻す（utf8.RuneStart が継続バイト 0b10xxxxxx を判定する）。
//
// max<=0 は制限なしを示す。切り詰め後は省略記号を追加するが、max が小さすぎて収まらない場合を除く。
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max が省略記号より短い場合は、省略記号を諦めて単純に切り詰め、結果が max を超えるのを防ぐ。
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine は複数行テキストを単一行に圧縮する：すべての空白をまとめ、その後文字数で切り詰める。
// IM メッセージのタイトル行に使う。要約には改行がよく含まれ、そのまま表/タイトルに入れるとレイアウトが崩れるため。
// max<=0 は長さ制限なしを示す。
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes は s を max 文字（バイトではない）以下に切り詰め、超過時は省略記号を追加する。
// max<=0 は制限なしを示す。
//
// TruncateBytes との違いはプラットフォームの基準：WeCom はバイト、Telegram は文字数で長さを制限する。
// 基準を間違えてもエラーにはならず、メッセージが想定よりはるかに短く切られるだけ（中国語 1 文字 = 3 バイトで、
// バイト単位で 4096 に切ると約 1365 文字しか残らない）。そのため両関数を保持し、チャネル別に選ぶ必要がある。
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML は HTML 断片を文字数で切り詰め、途中で切れたタグを生成しないことを保証する。
//
// HTML を文字で直接切り詰めると `<a href="htt` のような不完全なタグになり、プラットフォームの解析器が
// エラーで全体を拒否するか、後続の本文を属性値として飲み込んでしまう。ここでは先に文字で切り詰め、
// 末尾に閉じていない `<` があればその手前まで戻す。
//
// タグの対応付け（</b> などの補完）は行わない。Telegram の HTML 解析器が未閉鎖タグを自動で閉じるためで、
// 独自実装では属性の引用符、コメント、自己終了タグの処理が必要になり、複雑さと効果が釣り合わない。
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 末尾が `<` で始まる断片（最後の `<` より後ろに `>` がない）なら、`<` の手前まで戻す。
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 末尾が切れた HTML 実体参照（`&amp;` が `&amp` になった場合など）も同様に戻す。
	// 実体参照の断片は、実体参照のみ認識する解析器で**メッセージ全体**の拒否を招く可能性がある。上限を超えた
	// 集約メッセージはもともとよくあるため、それだけで通知全体を失う価値はない。
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount は予算内に**完全な形で**何件収まるかを計算し、集約メッセージを項目単位で詰めるために使う。
//
// 全体を描画してから切り詰めず項目単位にする理由：切り詰めると後半の項目が突然消えるが、
// 配信記録は配信済みとなるため、メッセージでも配信履歴でも分からず、
// 脆弱性がそのまま消えてしまう。項目単位で詰めると、収まらない項目はデータベース内に残って次のバッチになり、
// 呼び出し元が受け取る kept はこのメッセージで実際に届いた件数となる。
//
// 引数：maxSize<=0 は制限なし、reserve はメッセージの先頭/末尾用の確保量。
// size は計量を担当する（プラットフォームで基準が異なる：WeCom/DingTalk はバイト、Telegram は文字数。
// 基準を間違えてもエラーにはならず、中国語メッセージが上限よりはるかに小さく圧縮されるだけ）。
// render は idx 番目の項目を実際のテキストに描画する。長さは内容によって変わるため、推定に頼れない。
//
// 少なくとも 1 を返す（項目が残っている限り）。単一項目が極端に長くてもその1件は送信し、呼び出し元の
// 最終的な切り詰めに任せる。そうしないと、長すぎる脆弱性1件がバッチ全体を永久に止めてしまう。
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize は packItemCount の2種類の計量基準。名前を付けることで呼び出し箇所に
// 単なる func(s string) int クロージャーが現れるのを避ける。そうしないと、どの基準か一目で分かりづらい。
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine はアセット一覧を1行の表示テキストに描画し、limit 件を超えると残りを省略して総数を示す。
// 1つの脆弱性が数十のアセットに紐づくことがあり、すべて列挙するとメッセージが溢れる。
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, "、")
	}
	return strings.Join(assets[:limit], "、") + " ほか、合計" + itoa(len(assets)) + "件"
}

// itoa は strconv.Itoa の短い別名で、表示テキストの連結にのみ使い、各所での import strconv を避ける。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
