package notify

import (
	"fmt"
	"strings"
)

// このファイルは「Markdown 系」チャネル（DingTalk、WeCom）で共有するメッセージの描画を行う。
// Feishu はカード JSON、Telegram は HTML、メールは HTML を使い、それぞれのアダプターで描画する。

// maxAssetsShown はメッセージに列挙するアセットの最大数。1つの脆弱性が数十のアセットに紐づくこともあるが、
// すべて列挙するとメッセージが溢れ、情報的価値もない。4 番目以降のドメインを IM で読む人はいない。
const maxAssetsShown = 3

// maxSummaryRunes は要約を何文字に圧縮するかを表す。IM メッセージは「詳細の確認を促す」もので、
// 報告書そのものではなく、全内容はプラットフォーム内にある。
const maxSummaryRunes = 120

// markdownReservedBytes はメッセージの先頭（集約行 + レベル分布 + 必要なら切り詰め通知）
// と末尾（プラットフォームリンク）のために確保する。項目単位で詰める際にこの分を予算から差し引き、先頭と末尾の切り捨てを防ぐ。
// 先頭や末尾が切れると、読み手は「どのバッチで、未表示があと何件あるか」すら分からなくなる。
const markdownReservedBytes = 320

// markdownEscape は markdown のメタ文字をエスケープする。
//
// 必要な理由：脆弱性のタイトル、要約、種類、アセット表示名はすべて**信頼できないソース**に由来する。
// タイトルと要約はモデル出力（モデルはテスト対象の応答を読む）で、アセットの url はスキャンで
// 得た完全な URL（対象が制御できるクエリー文字列を含む）。エスケープしないと、タイトルが
//
//	ログイン箇所の SQL インジェクション\n[緊急：ここをクリックしてアカウントを検証](http://attacker.tld)
//
// という脆弱性は、セキュリティエンジニアの DingTalk/Feishu に**クリック可能な外部リンク**として描画される。
// `![](http://attacker.tld/beacon)` は描画時にクライアントに取得され、
// 「この脆弱性はすでに閲覧済み」と知らせると同時に閲覧者の IP を漏らす。悪意のない内容でも、注入された太字や
// 引用ブロックで、その下の重大な脆弱性が折り畳み線の外に押し出される。
//
// エスケープ対象には、見出し/リンク/強調/リスト/引用/取り消し線など、構造を変更したりクリック可能な
// 要素を生成する文字を含む。`\` は必ず最初に処理しないと、後から加えたバックスラッシュを再度エスケープしてしまう。
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText は信頼できないテキストを単一行に圧縮してエスケープし、markdown 本文に使う。
// 単一行化はエスケープと並ぶ対策のもう半分：改行自体が新しいリスト項目や引用ブロックを偽装でき、
// 文字のエスケープでは防げないため。
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle はメッセージのタイトル（IM プラットフォームのタイトル欄/カードタイトル）を返し、内容は**エスケープ前の原文**。
//
// ここで意図的にエスケープしないのは、このタイトルを4種類の文脈の描画処理が共有するため：markdown 本文、Telegram の
// HTML、Feishu カードの plain_text、汎用 Webhook の JSON とメールの件名。各文脈で
// エスケープ規則は異なり（markdown のエスケープを HTML に入れると見えるバックスラッシュが残り、JSON に入れると
// データを汚す）、各出力側が担当する必要がある。writeItem / feishuItemLines /
// telegramEscape を参照。以前、共有関数に markdown エスケープを加えた結果、Telegram メッセージに
// `\(1\)` のような見えるバックスラッシュが現れた。
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("脆弱性のまとめ · 合計%d件", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "脆弱性の通知"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody はメッセージ本文を描画し、本文と**実際に書き込んだ項目数**を返す。
//
// 戻り値 kept は今回の配信で実際に届いた項目数。呼び出し元はこれに基づき、先頭の kept 件のみを
// 配信済みとする。チャネルの長さ上限で入らなかった項目は、まとめて成功扱いにせず、
// 次のバッチへ回す必要がある。これこそ「黙った消失」の原因で、メッセージは切り詰められていても配信記録は全件配信済みとなり、
// 後半が未送信だと分かる場所がない。
//
// maxBytes<=0 は制限なしを示す。
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 単一メッセージは長すぎても送信する（最後の切り詰めで対応）。1件の脆弱性の一部の情報だけでも、
		// 何も送らないよりよい。
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[プラットフォームですべて表示](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro は集約メッセージの冒頭を描画する：時間範囲、件数、レベル分布。
// これがあれば、集約メッセージの受信者はプラットフォームを開かずに即時対応の必要性を判断できる。
//
// items は**実際に収まった**項目、total はこのバッチ全体の予定件数。両者が異なる場合は
// 「次のメッセージにあと何件あるか」を明示する必要がある。しないと読み手は先頭の数値を全件数と思い、
// その後ろの未送信の項目は画面上にまったく存在しないことになる。
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**過去%d分間に%d件の脆弱性を検出**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**新たに%d件の脆弱性を検出**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "（この通知は先頭%d件を表示。残り%d件は次の通知に続きます）", len(items), extra)
	}
	// レベル別の分布を示し、重大な項目の有無を一目で確認できるようにする。**このメッセージに実際に含まれる**
	// 項目だけを数え、「重大 3」とその下で数えられる項目が一致するようにする。
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem は単一の脆弱性項目を描画する。
//
// prefix は集約リストの番号に使い、single=true なら完全版（要約とリンクを含む）を描画する。
// 集約リストでは1行の要約のみ描画する。そうしないと 50 件の集約が長文書になってしまう。
//
// 外部由来の内容（タイトル/種類/アセット/要約）はすべて markdownText を通す：
// 単一行化 + エスケープ。リンクは管理者設定の public_base_url から組み立てるので信頼できない内容ではなく、
// クリック可能なリンクでなければならないため、そのまま出力する。
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 集約モード：1行で表示し、圧縮したアセットと要約を後ろに続ける。
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**状態の変更**：%s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**種類**：%s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**資産**：%s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**要約**：%s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[詳細を表示](%s)\n", it.DetailURL)
	}
}
