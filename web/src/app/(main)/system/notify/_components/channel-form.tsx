"use client";

import { CheckIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText / inputType はこのファイル内の値取得補助（コントロール描画と強く関連）で、channel-fields には置かない。
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText は任意の設定値を入力欄で使える文字列にする。
// config は JSON 由来で、値は string / number / boolean / array / null の場合があり、
// ここでは「テキスト欄に入れられるか」だけを扱い、具体的なシリアライズは buildConfig が担当する。
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType はフィールドの種類を input の type 属性へマッピングする。
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField はフィールド定義に応じて対応するコントロールを描画する。
//
// マスクフィールドの扱いだけが特別で、入力欄にはマスク値そのものを**表示せず**、1行の
// 「保存済み」だけを表示する。これにより画面上のルールは1つとなり、欄に文字があればユーザーの入力、
// 空欄なら空値になる。"__masked__:…abc123" を入力欄に入れると、ユーザーが自分で
// 消すべきプレースホルダーテキストだと考え、認証情報を誤消去しやすくなる。
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // バックエンドが返すマスク値："__masked__:…abc123" のような形で、末尾は元の値を識別できる断片。
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={def.label} />
        {def.label}
        {def.help && <span className="text-muted-foreground">（{def.help}）</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{def.label}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // コントロールはフィールドの種類で振り分ける。入れ子の三項演算子ではなく if チェーンを使うのは4種類を区別するためで、
  // 3段の三項演算子では読むときに括弧を数える必要がある。
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      保存しました{maskedTail ? `（末尾 ${maskedTail}）` : ""} · 新しい値で上書きし、空にするとこの項目を削除します
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{def.help}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{def.label}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary はフィルター条件を1行に要約し、カードを展開せずにそのチャネルの通知対象を分かるようにする。
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const parts: string[] = [];
  if (filter.min_severity) {
    parts.push(SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity)?.label ?? filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(`種類に含む ${filter.vulnclass_include.length} 語`);
  if (filter.vulnclass_exclude?.length) parts.push(`除外 ${filter.vulnclass_exclude.length} 語`);
  if (filter.task_ids?.length) parts.push(`${filter.task_ids.length} 件のタスク`);
  if (filter.asset_ids?.length) parts.push(`${filter.asset_ids.length} 件の資産`);
  if (filter.on_status_change) parts.push("状態の変更を含む");
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">すべての脆弱性</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
