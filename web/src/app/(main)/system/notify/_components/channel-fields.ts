// チャネルのフィールドテーブルと設定値の解析ツール。
//
// ページから分離するのは、これがビューではなく**データ**であり、各チャネルのフィールド、
// 各々に使うコントロール、フォームテキストと設定値（JSON）の双方向変換を記述するため。
// 個別ファイルにすることで、チャネル追加時はここだけ変更すればよく、ページ自体の変更は不要になる。
// チャネルの種類の表示名と紹介。文言だけに影響しバックエンドが知る必要はないため、フロントエンドに置く。
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Lark",
  wecom: "WeCom",
  webhook: "汎用 Webhook",
  telegram: "Telegram",
  email: "メール",
};

// 各チャネルの設定フィールド定義。
//
// バックエンドから schema を配布せず、フロントエンドのフィールドテーブルを意図的に保持する：バックエンドは
// Validate（必須/形式）だけを担当し、UI が必要とするレイアウトやコントロールの種類とは関心事が異なる。
// 唯一の結合点は secret_keys —— どのフィールドをパスワード欄にするかはバックエンドが示す。
// どの値が認証情報かはチャネル実装自身しか分からないため（企業版 WeChat は Webhook 全体が認証情報で、
// DingTalk はその中の secret だけ）。チャネル追加時にここで項目が欠けてもフォームが空白になるだけで、
// 黙って誤動作しない（下の hasFields が知らせる）。
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook アドレス",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "署名キー",
      kind: "password",
      help: "ボットの安全設定で「署名」を選択した場合に入力します。カスタムキーワードを使用する場合や安全設定が無効の場合は空欄にします",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook アドレス",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "署名検証キー", kind: "password", help: "ボットの署名検証が有効な場合に入力し、それ以外は空欄にします" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook アドレス",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "送信先 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "リクエストメソッド",
      kind: "select",
      options: [
        { value: "POST", label: "POST（本文あり）" },
        { value: "PUT", label: "PUT（本文あり）" },
        { value: "PATCH", label: "PATCH（本文あり）" },
        { value: "GET", label: "GET（本文なし）" },
      ],
    },
    { key: "headers", label: "カスタムリクエストヘッダー", kind: "kv", help: "1行に KEY=VALUE。例：Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "リクエスト本文のテンプレート",
      kind: "textarea",
      help:
        "空欄で組み込みテンプレートを使用します。変数：{{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}、" +
        "および range .Items 内の .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel。" +
        "文字列を挿入する際は {{.Xxx}} ではなく {{json .Xxx}} を使用してください。タイトル内の引用符で JSON が壊れるのを防ぎます。",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API アドレス",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "空欄で公式アドレスを使用します。独自の Bot API リバースプロキシを使用する場合に入力します",
    },
  ],
  email: [
    { key: "host", label: "SMTP サーバー", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "ポート",
      kind: "number",
      placeholder: "587",
      help: "587 は STARTTLS を使用します。465 は「暗黙的 TLS」を有効にしてください",
    },
    { key: "username", label: "アカウント", kind: "text" },
    { key: "password", label: "パスワード / 認証コード", kind: "password" },
    { key: "from", label: "差出人", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "宛先", kind: "list", help: "複数のアドレスはカンマで区切ります" },
    { key: "tls", label: "暗黙的 TLS", kind: "switch", help: "ポート465で有効、587では無効（自動で STARTTLS を使用）" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "制限なし" },
  { value: "low", label: "低以上" },
  { value: "medium", label: "中以上" },
  { value: "high", label: "高以上" },
  { value: "critical", label: "緊急のみ" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV は「各行 KEY=VALUE」のテキスト領域を解析する。
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs はカンマ/空白区切りの id 一覧を解析する。
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords は行/カンマ区切りのキーワード一覧を解析する（脆弱性の種類名には空白が入り得るため、行またはカンマで分ける）。
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
