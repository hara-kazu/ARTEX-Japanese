"use client";

import * as React from "react";

import { BellIcon, PlusIcon, SendIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { NotificationChannel, NotificationFilter, NotificationMeta } from "@/lib/types";

import {
  CHANNEL_FIELDS,
  type ChannelForm,
  emptyForm,
  KIND_LABEL,
  parseIDs,
  parseKeywords,
  parseKV,
  SEVERITY_OPTIONS,
} from "./_components/channel-fields";
import { ConfigField, FilterSummary } from "./_components/channel-form";
import { DeliveryList } from "./_components/delivery-list";
import { formatBacklog, StatTile } from "./_components/stat-tile";

// このページは調整のみを担当する：データ読み込み、フォーム状態の維持、インターフェース呼び出し。
// フィールド定義と解析は _components/channel-fields.ts、コントロールとフィルター概要は
// _components/channel-form.tsx、配信記録は _components/delivery-list.tsx——
// 分割したのは個別に読んで理解でき、1つのファイルに詰めるとこのページが約 1100 行になったため。
export default function NotifyPage() {
  const [meta, setMeta] = React.useState<NotificationMeta | null>(null);
  const [channels, setChannels] = React.useState<NotificationChannel[]>([]);
  const [tab, setTab] = React.useState<"channels" | "deliveries">("channels");

  const [open, setOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<NotificationChannel | null>(null);
  const [form, setForm] = React.useState<ChannelForm>(emptyForm("dingtalk"));
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);

  const [globalSaving, setGlobalSaving] = React.useState(false);
  const [baseURL, setBaseURL] = React.useState("");
  const [digestMin, setDigestMin] = React.useState("");

  const load = React.useCallback(() => {
    api
      .notifyMeta()
      .then((m) => {
        setMeta(m);
        setBaseURL(m.public_base_url);
        setDigestMin(m.digest_interval_min);
      })
      .catch((e) => toast.error("通知設定の読み込みに失敗しました：" + (e as Error).message));
    // チャネル一覧の読み込み失敗は表示する必要がある：黙って失敗すると「チャネルが1つもない」と表示され、
    // 設定が消えたと思わせてしまい、直接エラーを示すより不安を招く。
    api
      .notifyChannels()
      .then(setChannels)
      .catch((e) => toast.error("チャンネル一覧の読み込みに失敗しました：" + (e as Error).message));
  }, []);
  React.useEffect(() => {
    load();
  }, [load]);

  function setF(patch: Partial<ChannelForm>) {
    setForm((f) => ({ ...f, ...patch }));
  }
  function setCfg(key: string, value: unknown) {
    setForm((f) => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function openAdd() {
    setEditing(null);
    setForm(emptyForm(meta?.kinds[0]?.kind ?? "dingtalk"));
    setOpen(true);
  }

  function openEdit(ch: NotificationChannel) {
    setEditing(ch);
    // filter はバックエンドの Go 構造体で、常にオブジェクトへシリアライズされる（null にならない）ため、フォールバックは不要。
    const f = ch.filter;
    setForm({
      name: ch.name,
      kind: ch.kind,
      mode: ch.mode,
      enabled: ch.enabled,
      ratePerMin: String(ch.rate_per_min),
      // バックエンドが返す config の認証情報はマスク値；そのままフォームに入れ、送信時もそのまま返すと、
      // バックエンドはそれに基づいて保存済みの元の値を保持する。
      config: { ...ch.config },
      minSeverity: f.min_severity ?? "",
      includeText: (f.vulnclass_include ?? []).join("\n"),
      excludeText: (f.vulnclass_exclude ?? []).join("\n"),
      taskIDsText: (f.task_ids ?? []).join(","),
      assetIDsText: (f.asset_ids ?? []).join(","),
      onStatusChange: f.on_status_change ?? false,
    });
    setOpen(true);
  }

  // buildConfig はフォーム状態をチャネルの config に変換する。
  //
  // ルールは1つ、値は2種類：
  //   - マスク値（"__masked__..."）をそのまま返す → バックエンドは「このフィールドは未変更、保存済みの元の値を保持」と解釈する
  //   - その他はすべてユーザー入力として送信し、空文字列は「このフィールドを消去」を意味する
  //
  // 認証フィールドを特別扱いしない（「認証情報が空ならスキップ」など）のは、それではユーザーが誤設定した鍵を**消去できなくなる**ためで、
  // 画面上に「これを削除したい」と表せる操作がなくなる。現在のルールでは、
  // 入力欄のクリアはそのフィールドのクリアと同じで、意味が一意でユーザーが制御できる。
  // マスク値は入力欄には現れない（ConfigField を参照）ため、「欄に文字がある」は常に
  // 「ユーザーが能動的に入力した」を意味する。
  function buildConfig(): Record<string, unknown> {
    const defs = CHANNEL_FIELDS[form.kind] ?? [];
    const out: Record<string, unknown> = {};
    for (const d of defs) {
      const raw = form.config[d.key];
      if (d.kind === "switch") {
        out[d.key] = raw === true;
        continue;
      }
      if (typeof raw === "string" && raw.startsWith("__masked__")) {
        out[d.key] = raw;
        continue;
      }
      if (d.kind === "number") {
        const n = Number(raw);
        out[d.key] = Number.isFinite(n) && n > 0 ? n : 0;
        continue;
      }
      if (d.kind === "kv") {
        out[d.key] = parseKV(String(raw ?? ""));
        continue;
      }
      if (d.kind === "list") {
        out[d.key] = String(raw ?? "")
          .split(/[\s,，]+/)
          .map((s) => s.trim())
          .filter(Boolean);
        continue;
      }
      out[d.key] = String(raw ?? "").trim();
    }
    return out;
  }

  function buildFilter(): NotificationFilter {
    return {
      min_severity: form.minSeverity || undefined,
      vulnclass_include: parseKeywords(form.includeText),
      vulnclass_exclude: parseKeywords(form.excludeText),
      task_ids: parseIDs(form.taskIDsText),
      asset_ids: parseIDs(form.assetIDsText),
      on_status_change: form.onStatusChange,
    };
  }

  async function saveForm() {
    if (!form.name.trim()) {
      toast.error("チャンネル名を入力してください");
      return;
    }
    setSaving(true);
    try {
      const payload = {
        name: form.name.trim(),
        kind: form.kind,
        mode: form.mode,
        enabled: form.enabled,
        config: buildConfig(),
        filter: buildFilter(),
        rate_per_min: form.ratePerMin.trim() === "" ? undefined : Number(form.ratePerMin),
      };
      if (editing) {
        await api.notifyUpdateChannel(editing.id, payload);
        toast.success("保存しました");
        setOpen(false);
      } else {
        await api.notifyCreateChannel(payload);
        toast.success("チャンネルを追加しました");
        setOpen(false);
      }
      load();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function testChannel() {
    if (!editing) return;
    setTesting(true);
    try {
      const r = await api.notifyTestChannel(editing.id);
      toast.success(`テストメッセージを送信しました（${r.latency_ms} ms）。グループで確認してください`);
    } catch (e) {
      // バックエンドはチャネルの元のエラーをそのまま返し、設定を調べる唯一の手掛かりなので、そのまま表示する。
      toast.error("テストに失敗しました：" + (e as Error).message, { duration: 12000 });
    } finally {
      setTesting(false);
    }
  }

  async function removeChannel(ch: NotificationChannel) {
    try {
      await api.notifyDeleteChannel(ch.id);
      toast.success(`削除しました：${ch.name}`);
      setOpen(false);
      load();
    } catch (e) {
      toast.error("削除に失敗しました：" + (e as Error).message);
    }
  }

  async function toggleEnabled(ch: NotificationChannel) {
    try {
      await api.notifyUpdateChannel(ch.id, { enabled: !ch.enabled });
      load();
    } catch (e) {
      toast.error("操作に失敗しました：" + (e as Error).message);
    }
  }

  async function toggleGlobal(on: boolean) {
    setGlobalSaving(true);
    try {
      await api.setSettings({ notify_enabled: on });
      setMeta((m) => (m ? { ...m, enabled: on } : m));
      toast.success(on ? "通知を有効にしました" : "通知を一時停止しました");
    } catch (e) {
      toast.error("操作に失敗しました：" + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  async function saveGlobal() {
    setGlobalSaving(true);
    try {
      const patch: Record<string, unknown> = { notify_public_base_url: baseURL.trim() };
      const n = Number(digestMin);
      if (Number.isFinite(n) && n > 0) patch.notify_digest_interval_min = n;
      await api.setSettings(patch);
      toast.success("保存しました");
      load();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  const fields = CHANNEL_FIELDS[form.kind] ?? [];
  const secretKeys = new Set(meta?.kinds.find((k) => k.kind === form.kind)?.secret_keys ?? []);
  const defaultRate = meta?.kinds.find((k) => k.kind === form.kind)?.default_rate_per_min ?? 0;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">通知配信</h1>
          <p className="text-muted-foreground text-sm">
            脆弱性の検出時に DingTalk / Feishu / WeCom などへ通知します。チャンネルごとにタイミングと絞り込みを設定できます
          </p>
        </div>
        {meta && (
          // label ではなく div を使う：Switch 自体が aria-label を持ち、外側を label で包んでも
          // ネイティブコントロールに関連付かず、文字のクリックで切り替えられるように見せてしまう。
          <div className="flex shrink-0 items-center gap-2 text-sm">
            <span className="text-muted-foreground">全体の切り替え</span>
            <Switch
              checked={meta.enabled}
              disabled={globalSaving}
              onCheckedChange={toggleGlobal}
              aria-label="通知全体の有効化"
            />
          </div>
        )}
      </div>

      {meta && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StatTile label="チャンネル" value={`${meta.stats.channels_on} / ${meta.stats.channels}`} hint="有効 / 総数" />
          <StatTile label="今日の配信数" value={String(meta.stats.sent_today)} />
          <StatTile label="送信待ち" value={String(meta.stats.pending)} />
          <StatTile label="失敗" value={String(meta.stats.failed)} tone={meta.stats.failed > 0 ? "red" : undefined} />
          <StatTile
            label="最長の待機時間"
            value={formatBacklog(meta.stats.backlog_age_ms)}
            // 滞留時間は滞留件数よりはるかに有用：3 件の滞留でも 3 秒から 3 時間まであり得る。
            hint={meta.stats.backlog_age_ms > 5 * 60_000 ? "通知が滞留している可能性があります" : undefined}
            tone={meta.stats.backlog_age_ms > 5 * 60_000 ? "red" : undefined}
          />
        </div>
      )}

      <Card className="gap-3">
        <CardHeader>
          <CardTitle className="text-base">全体設定</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="n-base">リンク先アドレス</Label>
            <Input
              id="n-base"
              placeholder="https://artex.example.com"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">メッセージの「詳細を表示」ボタンのリンク先です。空欄の場合はボタンを表示しません。</p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="n-digest">集計間隔（分）</Label>
            <Input
              id="n-digest"
              type="number"
              min={1}
              max={1440}
              placeholder="30"
              value={digestMin}
              onChange={(e) => setDigestMin(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">「まとめて通知」のチャンネルにのみ適用します。</p>
          </div>
          <div className="sm:col-span-2">
            <Button onClick={saveGlobal} disabled={globalSaving}>
              全体設定を保存
            </Button>
          </div>
        </CardContent>
      </Card>

      <Tabs value={tab} onValueChange={(v) => setTab(v as "channels" | "deliveries")} className="flex flex-col gap-4">
        <TabsList>
          <TabsTrigger value="channels">チャンネル</TabsTrigger>
          <TabsTrigger value="deliveries">配信記録</TabsTrigger>
        </TabsList>

        <TabsContent value="channels">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <button
              type="button"
              onClick={openAdd}
              className="text-foreground/70 border-foreground/70 hover:bg-muted/60 hover:shadow-sm flex min-h-[130px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed transition"
            >
              <PlusIcon className="size-6" />
              <span className="text-sm">チャンネルを追加</span>
            </button>

            {channels.map((ch) => (
              <Card
                key={ch.id}
                onClick={() => openEdit(ch)}
                className="hover:border-primary/60 cursor-pointer gap-3 transition hover:shadow-sm"
              >
                <CardHeader>
                  <div className="flex items-center gap-2">
                    <BellIcon className="text-muted-foreground size-4 shrink-0" />
                    <CardTitle className="truncate text-base">{ch.name}</CardTitle>
                    {/* カード全体がクリック可能（編集へ移動）なので、この2つのコントロールは各自でバブリングを止める必要があり、
                        そうしないと切り替え/削除時に編集も発火する。stopPropagation はコントロール自体に
                        設定し、div で包まない：div で包むと「操作できそうに見えて
                        ロールのない」静的要素となり、a11y 警告が出るうえ、意味的にも不適切。 */}
                    <div className="ml-auto flex items-center gap-2">
                      <Switch
                        checked={ch.enabled}
                        onCheckedChange={() => toggleEnabled(ch)}
                        onClick={(e) => e.stopPropagation()}
                        aria-label="有効"
                      />
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="削除"
                        onClick={(e) => {
                          e.stopPropagation();
                          // void で Promise を明示的に破棄する：removeChannel 自身が catch して toast するため、
                          // ここで await は不要（onClick は async ではない）。
                          void removeChannel(ch);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="grid gap-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">{KIND_LABEL[ch.kind] ?? ch.kind}</Badge>
                    <Badge variant="outline">{ch.mode === "digest" ? "まとめて通知" : "リアルタイム"}</Badge>
                    {!ch.enabled && <Badge variant="outline">無効</Badge>}
                  </div>
                  <FilterSummary filter={ch.filter} />
                </CardContent>
              </Card>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="deliveries">
          <DeliveryList channels={channels} />
        </TabsContent>
      </Tabs>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="right" className="w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader>
            <SheetTitle>{editing ? editing.name : "通知チャンネルを追加"}</SheetTitle>
            <SheetDescription>
              {KIND_LABEL[form.kind] ?? form.kind}
              {defaultRate > 0 ? ` · 標準のレート制限 ${defaultRate} 件 / 分` : " · 制限なし"}
            </SheetDescription>
          </SheetHeader>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4">
            <div className="grid gap-4 py-4">
              <div className="grid gap-2">
                <Label>チャンネルの種類</Label>
                <Select
                  value={form.kind}
                  onValueChange={(v) => {
                    // 種類の変更は認証フィールド一式の変更なので、旧設定をマージしてはならない。
                    setF({ kind: v, config: {} });
                  }}
                  disabled={!!editing}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(meta?.kinds ?? []).map((k) => (
                      <SelectItem key={k.kind} value={k.kind}>
                        {KIND_LABEL[k.kind] ?? k.kind}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {editing && (
                  <p className="text-muted-foreground text-xs">
                    種類は変更できません。認証情報一式が変わるため、新しいチャンネルを作成してください。
                  </p>
                )}
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-name">チャンネル名</Label>
                <Input
                  id="n-name"
                  placeholder="緊急対応グループ / 日常報告グループ"
                  value={form.name}
                  onChange={(e) => setF({ name: e.target.value })}
                />
              </div>

              {fields.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  このチャンネルのフォームが未定義です（フロントエンドの CHANNEL_FIELDS に項目がありません）。定義を追加して再試行してください。
                </p>
              ) : (
                fields.map((d) => (
                  <ConfigField
                    key={d.key}
                    def={d}
                    value={form.config[d.key]}
                    isSecret={secretKeys.has(d.key)}
                    onChange={(v) => setCfg(d.key, v)}
                  />
                ))
              )}

              <div className="grid gap-2">
                <Label>通知のタイミング</Label>
                <Select value={form.mode} onValueChange={(v) => setF({ mode: v as "realtime" | "digest" })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="realtime">即時 · 脆弱性ごとに個別通知</SelectItem>
                    <SelectItem value="digest">まとめて · 指定間隔で1件に集約</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-muted-foreground text-xs">
                  「重大度が高以上は即時、ほかはまとめて」の場合、即時＋高以上のチャンネルと、まとめて＋重大度制限なしのチャンネルを作成してください。
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-rate">レート制限（件 / 分）</Label>
                <Input
                  id="n-rate"
                  type="number"
                  min={0}
                  placeholder={defaultRate > 0 ? String(defaultRate) : "0 = 制限なし"}
                  value={form.ratePerMin}
                  onChange={(e) => setF({ ratePerMin: e.target.value })}
                />
                <p className="text-muted-foreground text-xs">
                  空欄でチャンネルの既定値を使用し、0 は制限なしです。上限を超えてもメッセージは破棄せず、遅延して送信します。
                </p>
              </div>

              <div className="border-t pt-4">
                <p className="mb-3 text-sm font-medium">絞り込みルール（空欄で絞り込みなし）</p>
                <div className="grid gap-4">
                  <div className="grid gap-2">
                    <Label>最低重大度</Label>
                    <Select
                      value={form.minSeverity || "all"}
                      onValueChange={(v) => setF({ minSeverity: v === "all" ? "" : v })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {SEVERITY_OPTIONS.map((o) => (
                          <SelectItem key={o.value || "all"} value={o.value || "all"}>
                            {o.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-inc">通知に含める脆弱性の種類</Label>
                    <Textarea
                      id="n-inc"
                      placeholder={"SQL インジェクション\nコマンド実行"}
                      value={form.includeText}
                      onChange={(e) => setF({ includeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      1行に1キーワード。大文字と小文字を区別せず部分一致で判定します。空欄ですべての種類を対象にします。
                    </p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-exc">通知から除外する脆弱性の種類</Label>
                    <Textarea
                      id="n-exc"
                      placeholder={"情報漏えい"}
                      value={form.excludeText}
                      onChange={(e) => setF({ excludeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">除外を優先します。両方に一致する場合は除外します。</p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-tasks">タスク ID を限定</Label>
                    <Input
                      id="n-tasks"
                      placeholder="1, 2, 3"
                      value={form.taskIDsText}
                      onChange={(e) => setF({ taskIDsText: e.target.value })}
                    />
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-assets">資産 ID を限定</Label>
                    <Input
                      id="n-assets"
                      placeholder="10, 11"
                      value={form.assetIDsText}
                      onChange={(e) => setF({ assetIDsText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">タスク / 資産は空欄で制限なし。指定すると、脆弱性との関連が必要です。</p>
                  </div>
                  <div className="flex items-center gap-2 text-sm">
                    <Switch
                      checked={form.onStatusChange}
                      onCheckedChange={(v) => setF({ onStatusChange: v })}
                      aria-label="状態の変更を受信"
                    />
                    脆弱性の処理状態が変わった場合も通知（即時モードのみ）
                  </div>
                </div>
              </div>

              <div className="flex items-center gap-2 text-sm">
                <Switch checked={form.enabled} onCheckedChange={(v) => setF({ enabled: v })} aria-label="有効" />
                このチャンネルを有効にする
              </div>
            </div>

            <div className="flex gap-2 pt-2 pb-6">
              <Button onClick={saveForm} disabled={saving}>
                {editing ? "保存" : "追加"}
              </Button>
              {editing && (
                <Button variant="outline" onClick={testChannel} disabled={testing}>
                  <SendIcon /> テストメッセージを送信
                </Button>
              )}
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
