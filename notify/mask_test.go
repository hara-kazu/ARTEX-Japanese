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
		t.Fatalf("掩码值泄露了完整凭据: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("掩码值不应暴露地址主体: %q", got)
	}
	// 末尾 6 文字を残し、ユーザーがどのボットか識別できるようにする。
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("应保留末 6 位作为辨识提示: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("掩码值必须能被 IsMasked 识别: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 短い認証情報でも末尾 6 文字を公開すると、認証情報全体を公開することになる。
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("长度 %d 的凭据不应给出尾部提示，得到 %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("掩码值包含了原值: %q", got)
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
			t.Errorf("%s 应被掩码，得到 %q", k, s)
		}
	}
	// 認証情報以外のフィールドはそのまま保持する必要があり、しないと UI に表示できない。
	if masked["port"] != float64(587) {
		t.Errorf("非凭据字段 port 不应改动: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// チャネルの種類を識別できない場合は、認証情報を含む可能性のある元の内容を返さず、UI に空の設定を表示するほうを選ぶ。
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("未知渠道类型应返回空配置，得到 %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// マスクは表示層の動作であり、逆にデータベース内の実際の値を変えてはならない。
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig 修改了入参，会导致真实凭据被掩码值覆盖")
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
		t.Fatalf("掩码字段应保留库中原值，得到 %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("被修改的字段应生效，得到 %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("空串应清空该字段，得到 %v", got)
	}
	// 言及のないフィールドは保持する（部分更新の意味）。
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("未提及的字段应保留，得到 %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("未提及的字段应保留，得到 %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("已提及的字段应更新，得到 %v", got["port"])
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
			name: "通用 Webhook 改地址想沿用 Authorization 头",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram 改 base_url 想把 Bot Token 发到自己的端点",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "邮件改 SMTP 主机想交出密码",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "邮件关掉 TLS 也必须重新表态密码",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// マスク値 =「以前の認証情報を継続使用」であり、アドレス変更の文脈では同様に拒否する必要がある。
			name:        "回传掩码凭据 + 新地址",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "钉钉改 Webhook 想沿用加签密钥",
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
				t.Fatalf("改地址却未重新表态凭据，应当被拒绝；得到配置 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("应返回专门的错误类型以便接口给出可操作提示，得到 %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("应点名缺失的凭据键 %q，得到 %v", tc.wantMissing, target.Missing)
			}
			// エラーメッセージは操作者に修正方法を案内できること。
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("错误信息应提到 %q: %v", tc.wantMissing, err)
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
			name:     "只改名字（配置原样回传）",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "只改请求方法，地址与凭据都不动",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "换地址并**同时**给新凭据",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "换地址并显式声明不再需要凭据",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram 改 chat_id（不是目的地）",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "邮件改收件人（不是目的地）",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("合法编辑被误拦: %v", err)
			}
			if merged == nil {
				t.Fatal("应返回合并结果")
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
		t.Fatalf("端口值相同（仅类型不同）不应被判为地址变更: %v", err)
	}
	// 実際にポートを変更した場合は必ず阻止する。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("端口变更应被拦下")
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
		t.Fatalf("第一次保存被误拦: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("前提已变：空串应被 MergeConfig 删除——本用例要覆盖的正是「键消失之后」那一步")
	}

	// 2回目の保存：送信内容は前回と完全に同じで、ユーザーは何も変更していない。
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("第二次保存被误拦（用户什么都没改）: %v", err)
	}
	// 3回目も「1回だけ間違う」のではなく、安定して保存できることを確認する。
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("第三次保存被误拦: %v", err)
	}
	// 認証情報は最後まで保持され、空文字列のロジックに巻き込まれて消去されないこと。
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token 应沿用原值，得到 %v", got)
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
		t.Fatal("从官方地址换到自建地址必须要求重新填写 Token")
	}

	// 方向2：自前アドレスをクリアする（= 公式 API に戻す）のもアドレス変更にあたる。
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("清空自建地址（换回官方 API）同样是地址变更，必须要求重新填写 Token")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys と同じく、チャネルが送信先キーの宣言を忘れると、PrepareConfigUpdate は保護できない。
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("渠道 %s 未声明目的地键，改地址带出凭据的防护对它无效", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("渠道 %s 未声明凭据键", kind)
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
			t.Errorf("渠道 %s 未在测试中登记掩码预期", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("渠道 %s 未声明任何凭据字段，其配置会明文回显", kind)
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
		t.Fatal("结构体内部夹带掩码哨兵应被拒绝（否则会把字面量存进库）")
	}
	// オブジェクト全体の送信（実際の新しい値）は通常どおり受け入れる。
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("正常提交新请求头不应被拦: %v", err)
	}
}
