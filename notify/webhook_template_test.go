package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// このファイルは汎用 Webhook テンプレートの**能力の境界**を固定する。
//
// 本パッケージ内で唯一「ユーザー提供の文字列をコードとして評価する」場所なので、できることと
// できないことを明確にし、テストでその性質を固定する必要がある。そうしないと、将来ついでにテンプレートコンテキストに
// メソッドを加えたり、FuncMap に readFile を加えたりして、diff では無害な小さな関数に
// しか見えないまま、能力の範囲が黙って広がってしまう。

// TestTemplateContextHasNoMethods は最も重要な条件。
//
// text/template は公開メソッドを呼び出す（{{.Foo}} はフィールド取得もメソッド呼び出しも可能）。そのためテンプレートコンテキストから
// 公開メソッドを持つ**いかなる**型にもアクセスできると、そのメソッドをテンプレート作成者に公開したことになる。
// この機能のコンテキストは意図的に純粋なデータのみである（公開フィールドのみ、メソッドなし）。
//
// この条件が失敗したら、webhookTemplateData / webhookItem に誰かがメソッドを追加したことを意味する。
// 許可を決める前に、そのメソッドをテンプレートで使い、公開したくないものを読み取れないかをよく考える。
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s 暴露了 %d 个方法（%s）：text/template 可以调用它们，"+
				"等于把这些方法的能力开放给了模板作者", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal はテンプレートに公開する関数の集合を固定する。
//
// FuncMap に関数が1つ増えるごとに能力が1つ増える。現在は json / jsons のみで、値を
// JSON 断片にシリアライズする用途に限り、ファイル読み取り、リクエスト送信、コマンド実行はできない。
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("模板函数集合变了：得到 %v，期望 %v。新增函数前请确认它不会扩大能力面"+
			"（不能读写文件、不能发起网络请求、不能执行命令）", got, want)
	}
}

// TestTemplateCannotReachUnknownData はテンプレートからの範囲外アクセスを検証する：
// 存在しないものへのアクセスは何かを返すのでなく失敗する必要があり、失敗メッセージには内部データを含めてはならない。
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("访问不存在的字段应报错")
	}
	// エラーにテンプレートコンテキスト内の実際の内容（脆弱性のタイトル/要約）が現れてはならない。
	for _, leak := range []string{"SQL注入", "参数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("模板错误泄露了消息内容 %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently はテンプレートの誤りが設定エラーであり、再試行しても自然に直らないことを検証する。
// 再試行可能と判定すると、不正なテンプレート1つで各配信が無駄に3回バックオフすることになる。
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("模板语法错误应在保存时就被拦下")
	}
	// 検証を回避して直接配信しても、繰り返し再試行せず永続的な失敗と判定する必要がある。
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("坏模板应判永久失败，得到 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON は「テンプレートの描画結果は正しい JSON でなければならない」という制約を検証する。
// これにより「テンプレートでプレーンテキストを生成して他のプロトコルを発火させる」という使い方も防ぐ。
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 正しいテンプレートは通る。
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("合法模板应通过校验: %v", err)
	}
	// JSON 以外を描画したら、必ず拒否する（そのまま送信しない）。
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("渲染出非 JSON 应判永久失败，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "有効な JSON") {
		t.Errorf("错误信息应说明是 JSON 问题，得到 %v", err)
	}
}
