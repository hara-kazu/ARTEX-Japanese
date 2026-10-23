package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("マスク値が認証情報全体を漏えいしています: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("マスク値はアドレスの主要部分を公開するべきではありません: %q", got)
	}
	// 末尾 6 文字を残し、ユーザーがどのボットか識別できるようにする。
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("識別用のヒントとして末尾 6 桁を保持するべきです: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("マスク値は IsMasked で認識できる必要があります: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 短い認証情報でも末尾 6 文字を公開すると、認証情報全体を公開することになる。
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("長さ %d の認証情報では末尾のヒントを示すべきではありません。実際の値: %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("マスク値に元の値が含まれています: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s はマスクされるべきです。実際の値: %q", k, s)
		}
	}
	// 認証情報以外のフィールドはそのまま保持する必要があり、しないと UI に表示できない。
	if masked["port"] != float64(587) {
		t.Errorf("認証情報ではないフィールド port は変更するべきではありません: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// チャネルの種類を識別できない場合は、認証情報を含む可能性のある元の内容を返さず、UI に空の設定を表示するほうを選ぶ。
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("未知のチャネル種類は空の設定を返すべきです。実際の値: %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// マスクは表示層の動作であり、逆にデータベース内の実際の値を変えてはならない。
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig が入力引数を変更しており、実際の認証情報がマスク値で上書きされます")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// ユーザーは method のみ変更し、ブラウザーはマスク値+新しい method を送信する。
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("マスクされたフィールドは保存先の元の値を保持するべきです。実際の値: %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("変更したフィールドは反映されるべきです。実際の値: %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("空文字列はそのフィールドを消去するべきです。実際の値: %v", got)
	}
	// 言及のないフィールドは保持する（部分更新の意味）。
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("指定していないフィールドは保持するべきです。実際の値: %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("指定していないフィールドは保持するべきです。実際の値: %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("指定したフィールドは更新するべきです。実際の値: %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap は本パッケージで最も重要なセキュリティの不変条件：
// **送信先アドレスを変更しても、以前の認証情報を持ち込めてはならない**。
//
// これらのケースは、まさに攻撃の形の入力（アドレスだけ変更し、認証情報は指定しない）を使い、
// 「防御ロジックへの正しい入力」ではない。後者のみテストすると、防御が効かなくてもすべて成功してしまう。
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing は名指しされる予定の認証情報キー。
		wantMissing string
	}{
		{
			name: "汎用 Webhook でアドレス変更後も Authorization ヘッダーを使おうとする",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram の base_url を変えて Bot Token を自分のエンドポイントへ送ろうとする",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "メールの SMTP ホストを変えてパスワードを渡そうとする",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "メールの TLS を無効にした場合もパスワードの再指定が必要",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// マスク値 =「以前の認証情報を継続使用」であり、アドレス変更の文脈では同様に拒否する必要がある。
			name:        "マスク済みの認証情報と新しいアドレスを返す",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk の Webhook を変えて署名キーを引き継ごうとする",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("アドレスを変更し、認証情報の扱いを再指定しない場合は拒否されるべきです。実際の設定: %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("API が対処可能な案内を示せるよう、専用のエラー型を返すべきです。実際の値: %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("不足している認証情報のキー %q を明示するべきです。実際の値: %v", tc.wantMissing, target.Missing)
			}
			// エラーメッセージは操作者に修正方法を案内できること。
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("エラー情報には %q を含めるべきです: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits は逆方向のケース：通常の編集を誤って阻止してはならない。
// そうしないと、この防御は「面倒すぎる」ため回避されたり削除されたりする。
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "名前だけ変更（設定はそのまま返す）",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "リクエスト方法だけ変更し、アドレスと認証情報はそのまま",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "アドレス変更と**同時に**新しい認証情報を指定",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "アドレス変更時に認証情報が不要になったと明示",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram の chat_id を変更（送信先ではない）",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "メールの受信者を変更（送信先ではない）",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("有効な編集が誤ってブロックされました: %v", err)
			}
			if merged == nil {
				t.Fatal("マージ結果を返すべきです")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance は誤判定しやすい細部を検証する：
// フロントエンドが送信するポートは JSON number（float64）、データベースから読み戻しても float64 だが、
// 2つの値の型が異なる可能性がある（int vs float64 など）。== で比較すると「未変更」を「変更あり」と判定し、
// 名前しか変更していないユーザーに「パスワードを再入力してください」と表示する。誤警告はこの防御への信頼を失わせる。
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 同じポートを int として送信する。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("ポートの値が同じ場合（型だけが異なる場合）はアドレス変更と判定するべきではありません: %v", err)
	}
	// 実際にポートを変更した場合は必ず阻止する。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("ポートの変更はブロックされるべきです")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination は「空欄を許す
// 送信先フィールド」の経路を検証する：Telegram の base_url の空欄は公式 API アドレスの使用を意味する。
//
// 以前は2回目の保存からチャネルの保存が永久に失敗していた：
//
//	新規作成時に base_url:"" をデータベースへ保存（作成経路はフロントエンド送信の config を直接保存し、MergeConfig を通さない）
//	→ 初回保存で MergeConfig が空文字列を明示的なクリアとしてキーを delete する
//	→ 2回目の保存でも incoming は "" だが、stored にキーがなく、「アドレス変更」と判定
//	→ bot_token はマスクした応答値 → 400「送信先アドレスが変更されたため、認証情報フィールドも再入力してください」
//
// ユーザーは何も変更していないのに、それ以降 Bot Token を再度貼り付けない限り保存できない。
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// フロントエンドの buildConfig() は、このチャネルの各フィールド定義に対し値を1つ送信する：認証情報にはマスク値を設定し、
	// 空のテキスト欄は空文字列を送信する。ここでは「変更されたキー」だけでなく、その出力全体を再現する。
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 初回保存：チャネル名のみ変更し、config はそのまま返す。
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("1 回目の保存が誤ってブロックされました: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("前提が変わっています: 空文字列は MergeConfig で削除されるべきです——このテストケースが対象とするのは、まさに「キーが消失した後」のステップです")
	}

	// 2回目の保存：送信内容は前回と完全に同じで、ユーザーは何も変更していない。
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("2 回目の保存が誤ってブロックされました（ユーザーは何も変更していません）: %v", err)
	}
	// 3回目も「1回だけ間違う」のではなく、安定して保存できることを確認する。
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("3 回目の保存が誤ってブロックされました: %v", err)
	}
	// 認証情報は最後まで保持され、空文字列のロジックに巻き込まれて消去されないこと。
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token は元の値を引き継ぐべきです。実際の値: %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges は前のケースと対になる
// アサーション：空文字列と「キーなし」を同等と扱っても、実際のアドレス変更を併せて許可しては**ならない**。
// どちらの方向も実際に認証情報を外部送信する経路である。Telegram の Bot Token は URL パスに含まれるので、
// base_url を変えると Token を新しいアドレスに送ることになる。
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 方向1：「空」（公式アドレス）から自前アドレスに変更。
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("公式アドレスから独自のアドレスへ変更する場合は Token の再入力を必須にする必要があります")
	}

	// 方向2：自前アドレスをクリアする（= 公式 API に戻す）のもアドレス変更にあたる。
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("独自のアドレスを消去する場合（公式 API に戻す場合）もアドレス変更であり、Token の再入力を必須にする必要があります")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys と同じく、チャネルが送信先キーの宣言を忘れると、PrepareConfigUpdate は保護できない。
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("チャネル %s が宛先キーを宣言しておらず、アドレス変更による認証情報の流出防止策が機能しません", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s が認証情報のキーを宣言していません", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// コンパイラーがすべてのチャネルに SecretKeys の実装をすでに強制するが、ここでは「マスクに対応しないチャネルがない」ことを
	// 再確認する。空スライスを返すチャネルは、その認証情報を平文でブラウザーへ返すことを意味する。
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("チャネル %s のマスクの期待値がテストに登録されていません", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s が認証情報のフィールドを宣言しておらず、設定が平文で返されます", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer は監査で指摘された抜け穴を検証する：
// マスクの番兵値を**文字列以外**の構造（webhook.headers オブジェクトなど）に入れると、
// MergeConfig は「文字列で、かつ接頭辞付き」のみをマスクと解釈するため、リテラルの "__masked__" が
// 実際のヘッダー値としてデータベースに保存され、以後の認証が黙って無効化され、エラーも一切出ない。
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// オブジェクト内部にマスクの番兵値を入れる。
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("構造体の内部にマスクのセンチネル値を含む場合は拒否されるべきです（そうしないとリテラル値が保存先に書き込まれます）")
	}
	// オブジェクト全体の送信（実際の新しい値）は通常どおり受け入れる。
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("新しいリクエストヘッダーの通常の送信はブロックされるべきではありません: %v", err)
	}
}
