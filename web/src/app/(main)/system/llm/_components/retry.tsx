"use client";

// LLM 再試行設定の共通部品：5層の再試行それぞれの「回数 + 間隔」。
//
// 5層は内側から外側へ：接続(SDK) → 空応答(SDK) → 同じ provider の安全ウィンドウ → 巡回のサーキットブレーク → インテント再実行。
// 最初の3層はエンドポイントに付随するため、モデル設定ごとにグローバル既定値を上書きできる；後ろの2層はプロセス単位で、グローバル設定1つだけ。
//
// 全入力は同じ「空欄 = 未設定」の意味に従い、バックエンドの db.RetryRule と一致する：
//   回数   空/0 = 組み込み既定値を使う | -1 = この層の再試行を無効化 | >0 = この回数を使う
//   間隔   空/0 = この層の元の指数バックオフを使う | >0 = この固定ミリ秒間隔に変更

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** この層の再試行が起きる場所と実行者 */
  where: string;
  /** この層に来るエラーの種類——ステータスコードまで具体的に示し、推測させない */
  trigger: string;
  /** 似ていてもこの層を【使わない】エラー。設定して反応がなくても bug と誤解しないようにする */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 回数が空の場合の既定値。プレースホルダー用 */
  defAttempts: number;
  /** 間隔が空の場合の既定戦略。プレースホルダー用 */
  defInterval: string;
  /** 回数 -1 の意味 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "接続の再試行",
    where: "SDK · HTTP 200 受信前",
    trigger:
      "接続不可や HTTP 200 受信前のエラー：接続リセット、読み書きタイムアウト、DNS エラーなどのネットワーク障害、および HTTP 408、429、500、502、503、504。",
    skips: "その他のステータスコード（400 / 401 / 403 / 404 / 413 / 422 など）は確定的な拒否で、再送しても同じように失敗するため、直ちにエラーを上位へ返します。",
    desc: "同じリクエストをそのまま再送します。ストリーム開始後（HTTP 200 受信後）の切断は、この層の対象外です。",
    attemptsLabel: "再試行回数",
    defAttempts: 3,
    defInterval: "0.5s → 1s → 2s の指数バックオフ（上限8s）",
    offHint: "-1 = 再試行せず、直ちにエラーを返す",
  },
  empty: {
    title: "空のレスポンスの再試行",
    where: "SDK · openai 形式のみ",
    trigger:
      "HTTP 200 で finish_reason が正常な stop でも、内容のブロックが1つもない応答です。ゲートウェイの空フレーム、思考フィールドの欠落、生成の一時的な不具合などで発生します。",
    skips: "max_tokens による打ち切りは対象外です（出力上限の調整が必要で、再送しても再発します）。",
    desc: "prompt全体を再送するため、長いコンテキストではコストが高くなります。回数を増やしすぎないでください。",
    attemptsLabel: "再試行回数",
    defAttempts: 2,
    defInterval: "0.5s → 1s → 2s の指数バックオフ（上限8s）",
    offHint: "-1 = 空のレスポンスをそのまま返す",
  },
  stream: {
    title: "同じprovider内での安全な再試行",
    where: "アプリ側 · 出力の引き渡し前",
    trigger:
      "HTTP 200 受信後に、途中切断、プロバイダーの overloaded、ストリーム内の 429 / 5xx エラーが発生し、呼び出し元にまだ1tokenも渡していない場合です。",
    skips:
      "利用枠不足（402 / insufficient_quota は設定切り替え）、コンテキスト超過（413 / context length は圧縮）、400 / 401 / 403 / 404 / 422 の確定的な拒否は再試行しません。",
    desc: "同じ設定で同じリクエストを再送します。出力をまだ渡していないため、モデルの出力やツールの実行は重複しません。",
    attemptsLabel: "再試行回数",
    defAttempts: 2,
    defInterval: "0.5s → 1s の指数バックオフ（上限4s）",
    offHint: "-1 = ストリーム切断を外側のインテント再実行に渡す",
  },
  breaker: {
    title: "切り替え時のサーキットブレーカー",
    where: "アプリ側 · プロセス全体で共有",
    trigger:
      "一時的な障害（429、5xx、ネットワークエラー）が連続してしきい値に達すると遮断します。残高不足（402）、キー無効（401 / 403）、モデル不存在（404）などの確定的な失敗は初回で遮断します。",
    skips: "1回成功すると失敗数をリセットします。一時的な失敗が断続的に起こるだけでは遮断しません。",
    desc: "遮断後はクールダウンに入り、その間は切り替え対象から除外します。状態はデータベースに保存され、再起動後も保持されます。",
    attemptsLabel: "遮断する連続失敗回数",
    defAttempts: 3,
    defInterval: "1min → 5min → 30min の段階制",
    offHint: "-1 = 一時的な障害では遮断しない（確定的な失敗では遮断）",
  },
  intent: {
    title: "インテントの再実行",
    where: "アプリ側 · プロセス全体で共有",
    trigger:
      "前の層で回復できず、worker が model_error で終了した場合です。内部の再試行を使い切った場合や、出力開始後の切断（安全に再送できず、最初から実行が必要）が対象です。",
    skips: "利用枠不足は設定切り替えで処理し、ここでは再実行しません。タスクの一時停止 / 終了 / 終了処理に入ると、バックオフを待たずに中断します。",
    desc: "インテント全体を最初から実行します。最も外側の処理のため、再実行ごとに内部の各層の再試行回数も再び適用されます。",
    attemptsLabel: "再実行回数",
    defAttempts: 2,
    defInterval: "固定3s",
    offHint: "-1 = 再実行せず、このインテントを blocked と判定",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** ミリ秒を分かりやすく表し、入力欄の横に表示してゼロを数えずに済むようにする。 */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 制御された数値入力：空文字列 ↔ 0、途中状態（"-"、"1e"）はそのままローカルに残し、親へ影響させない。 */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 親側の値一式が変わった（戦略を取得、設定切り替え）場合は追従する；自分で入力しているときはここを通らず、
  // そのときの value はローカルテキストを parse した結果と既に一致しているため。
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 1層の再試行の2つの調整項目。idPrefix は同じページに複数現れても label の htmlFor を保つために使う。 */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 設定ドロワー内のコンパクト版：展開する説明を省き、「この層に来るエラー」の1文だけ残す */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* この層に来るエラーをステータスコードまで具体的に示す——設定しても効果が見えない場合、多くはそのエラーがこの層に属していない。 */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">発動条件</span>：{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">この層の対象外</span>：{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`デフォルト ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            間隔（ms）
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="標準のバックオフ"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `固定 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">空欄 = 既定値を使用；{meta.offHint}。</p>}
    </div>
  );
}

/** モデル設定ドロワー内の3層の上書き（エンドポイントに付随する3層）。 */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">再試行の上書き設定</Label>
        <p className="text-muted-foreground text-xs">
          この設定にのみ適用し、「再試行とバックオフ」の全体設定を上書きします。空欄は全体設定に従い、回数 -1 はその層を無効にします。間隔を指定すると指数バックオフの代わりに固定間隔を使用します。遮断とインテント再実行はプロセス全体の設定のため、全体設定画面でのみ変更できます。
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「再試行とバックオフ」tab：5層のグローバル既定値。 */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`再試行ポリシーの読み込みに失敗しました：${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // バックエンドは範囲外の値を範囲内に収めて返すため、返却値で直接更新し、見える値を保存値と一致させる。
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("保存し、即時反映しました（実行中の呼び出しは以前のパラメーターを使用します）");
    } catch (e) {
      toast.error(`保存に失敗しました：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> 再試行ポリシーを読み込んでいます…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        モデル呼び出しの失敗は、内側から外側へ次の5層で再試行します：
        <span className="text-foreground"> 接続 → 空のレスポンス → 同じproviderの安全な再試行 → 切り替え時の遮断 → インテント再実行</span>
        。内側を使い切ってから外側へ進むため、回数は
        <span className="text-foreground">掛け算</span>
        になります。各層を最大にすると、1回の障害で数十回のリクエストが発生し得ます。すべて空欄なら既定値を使用し、従来と同じ動作です。最初の3層はモデル設定ごとに上書きできます。
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          保存
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          すべて既定値に戻す
        </Button>
      </div>
    </div>
  );
}
