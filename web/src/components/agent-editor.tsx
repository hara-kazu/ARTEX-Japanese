"use client";

import * as React from "react";
import { toast } from "sonner";
import { EyeIcon, GitCompareIcon, InfoIcon, PencilIcon, RotateCcwIcon, SaveIcon, Trash2Icon, XIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Textarea } from "@/components/ui/textarea";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Markdown } from "@/components/markdown";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import type { Agent, AgentDetail, AgentTrigger, MCPServer, PromptVar, PromptVersion, Settings, SkillItem, Tool } from "@/lib/types";

// Traffic tools are host tools gated by the global トラフィックキャプチャ switch: bindable, but
// only usable when capture is on. Keep this list in sync with traffic.SeedToolMetas.
const TRAFFIC_TOOL_KEYS = new Set(["traffic_search", "traffic_get"]);

// AgentEditor is the tabbed editor for one agent, used inside the agents-page
// drawer (and reused full-page for deep links). Tabs: 設定とプロンプト / MCP / Skill /
// Tools. Config + prompt save as before; visibility + tool bindings toggle live.
export function AgentEditor({ agentKey, onSaved }: { agentKey: string; onSaved?: () => void }) {
  const [detail, setDetail] = React.useState<AgentDetail | null>(null);
  const [versions, setVersions] = React.useState<PromptVersion[]>([]);
  const [variables, setVariables] = React.useState<PromptVar[]>([]);
  const [mcp, setMcp] = React.useState<MCPServer[]>([]);
  const [skills, setSkills] = React.useState<SkillItem[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [viewVer, setViewVer] = React.useState<PromptVersion | null>(null);
  const [diffVer, setDiffVer] = React.useState<PromptVersion | null>(null);

  const [prompt, setPrompt] = React.useState("");
  const [mcpVisible, setMcpVisible] = React.useState<number[]>([]);
  const [skillVisible, setSkillVisible] = React.useState<string[]>([]);
  const [preview, setPreview] = React.useState("");
  const [maxTurns, setMaxTurns] = React.useState("0");
  const [runSecs, setRunSecs] = React.useState("600");
  // "" = 継承(未バインド)；それ以外は profile id の文字列
  const [llmProfileId, setLlmProfileId] = React.useState("");
  const [llmProfiles, setLlmProfiles] = React.useState<NonNullable<AgentDetail["llm_profiles"]>>([]);
  const [webSearch, setWebSearch] = React.useState(false);
  const [interactiveShell, setInteractiveShell] = React.useState(false);
  const [wrapup, setWrapup] = React.useState("");
  const [wrapupDefault, setWrapupDefault] = React.useState("");
  const [wrapupTurns, setWrapupTurns] = React.useState("0");
  const [wrapupTurnsDefault, setWrapupTurnsDefault] = React.useState(5);
  // タスク単位のタイムアウト終了プロンプト(worker/planner のみ)
  const [ttSupported, setTtSupported] = React.useState(false);
  const [ttWrapup, setTtWrapup] = React.useState("");
  const [ttWrapupDefault, setTtWrapupDefault] = React.useState("");
  const [ttTurns, setTtTurns] = React.useState("0");
  const [ttTurnsDefault, setTtTurnsDefault] = React.useState(5);
  const [settings, setSettings] = React.useState<Settings | null>(null);

  React.useEffect(() => {
    api.mcpServers().then(setMcp).catch(() => {});
    api.skills().then(setSkills).catch(() => {});
    api.tools().then(setTools).catch(() => {});
    api.settings().then(setSettings).catch(() => {});
  }, []);
  // global gates: traffic tools need トラフィックキャプチャ, web search needs the master switch.
  const captureOn = !!settings?.traffic_capture;
  const webSearchGlobalOn = !!settings?.web_search_enabled;

  const reload = React.useCallback(() => {
    api
      .getAgent(agentKey)
      .then((d) => {
        setDetail(d);
        setPrompt(d.prompt ?? "");
        setVariables(d.variables ?? []);
        setVersions(d.versions ?? []);
        setMcpVisible(d.visibility?.mcp ?? []);
        setSkillVisible(d.visibility?.skill ?? []);
        setMaxTurns(String(d.agent?.max_turns ?? 0));
        setRunSecs(String(d.agent?.run_seconds ?? 600));
        setLlmProfileId(d.agent?.llm_profile_id != null ? String(d.agent.llm_profile_id) : "");
        setLlmProfiles(d.llm_profiles ?? []);
        setWebSearch(!!d.agent?.web_search);
        setInteractiveShell(!!d.agent?.interactive_shell);
        setWrapup(d.wrapup_prompt ?? "");
        setWrapupDefault(d.wrapup_default ?? "");
        setWrapupTurns(String(d.wrapup_max_turns ?? 0));
        setWrapupTurnsDefault(d.wrapup_max_turns_default ?? 5);
        setTtSupported(!!d.task_timeout_wrapup_supported);
        setTtWrapup(d.task_timeout_wrapup_prompt ?? "");
        setTtWrapupDefault(d.task_timeout_wrapup_default ?? "");
        setTtTurns(String(d.task_timeout_wrapup_max_turns ?? 0));
        setTtTurnsDefault(d.task_timeout_wrapup_max_turns_default ?? 5);
      })
      .catch(() => setDetail(null))
      .finally(() => setLoaded(true));
  }, [agentKey]);
  React.useEffect(() => {
    reload();
  }, [reload]);

  async function doPreview() {
    try {
      const r = await api.previewAgentPrompt(agentKey, prompt);
      setPreview(r.error ? "描画エラー：" + r.error : r.rendered);
    } catch (e) {
      setPreview("プレビューに失敗しました：" + (e as Error).message);
    }
  }
  async function savePrompt() {
    try {
      const r = await api.saveAgentPrompt(agentKey, prompt);
      toast.success(`バージョンとして保存しました：v${r.version}`);
      reload();
      onSaved?.();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function resetPrompt() {
    try {
      const r = await api.resetAgentPrompt(agentKey);
      toast.success(`組み込みの既定値に戻しました（v${r.version}）`);
      reload();
    } catch (e) {
      toast.error("復旧に失敗しました：" + (e as Error).message);
    }
  }
  async function saveWrapup() {
    try {
      const turns = Math.max(0, Math.floor(Number(wrapupTurns) || 0));
      await api.saveAgentWrapup(agentKey, wrapup, turns);
      toast.success(wrapup.trim() || turns > 0 ? "終了処理の設定を保存しました（次回の実行から適用）" : "クリアしました。組み込みの既定値を使用します");
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function resetWrapup() {
    try {
      await api.resetAgentWrapup(agentKey);
      toast.success("組み込みの既定値に戻しました");
      reload();
    } catch (e) {
      toast.error("復旧に失敗しました：" + (e as Error).message);
    }
  }
  async function saveTaskTimeoutWrapup() {
    try {
      const turns = Math.max(0, Math.floor(Number(ttTurns) || 0));
      await api.saveAgentTaskTimeoutWrapup(agentKey, ttWrapup, turns);
      toast.success("タスクのタイムアウト時の終了処理を保存しました（次回の実行から適用）");
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function resetTaskTimeoutWrapup() {
    try {
      await api.resetAgentTaskTimeoutWrapup(agentKey);
      toast.success("組み込みの既定値に戻しました");
      reload();
    } catch (e) {
      toast.error("復旧に失敗しました：" + (e as Error).message);
    }
  }
  async function saveConfig() {
    try {
      // この agent に実際に表示されるフィールドだけを送信し、未表示項目(goals の max_turns など)が既定値で上書きされるのを防ぐ。
      const patch: Parameters<typeof api.saveAgentConfig>[1] = {
        llm_profile_id: llmProfileId === "" ? null : Number(llmProfileId),
      };
      if (showConfig) {
        patch.max_turns = Math.max(0, Math.floor(Number(maxTurns) || 0));
        patch.run_seconds = Math.max(0, Math.floor(Number(runSecs) || 0));
      }
      if (showWebSearch) patch.web_search = webSearch;
      if (showInteractiveShell) patch.interactive_shell = interactiveShell;
      await api.saveAgentConfig(agentKey, patch);
      toast.success("実行設定を保存しました（即時反映）");
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  // applyVis optimistically updates, persists, and toasts success/failure. On
  // failure it reverts to the prior selection so the UI never lies about state.
  async function applyVis(nextMcp: number[], nextSkill: string[], okMsg: string) {
    const prevMcp = mcpVisible;
    const prevSkill = skillVisible;
    setMcpVisible(nextMcp);
    setSkillVisible(nextSkill);
    try {
      await api.setAgentVisibility(agentKey, nextMcp, nextSkill);
      toast.success(okMsg);
      onSaved?.(); // refresh the list so the card's MCP/Skill counts stay in sync
    } catch (e) {
      setMcpVisible(prevMcp);
      setSkillVisible(prevSkill);
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  function toggleMcp(id: number) {
    const on = mcpVisible.includes(id);
    const name = mcp.find((m) => m.id === id)?.name ?? String(id);
    applyVis(
      on ? mcpVisible.filter((x) => x !== id) : [...mcpVisible, id],
      skillVisible,
      `${on ? "公開を解除しました：" : "公開を有効にしました："} MCP「${name}」`,
    );
  }
  function toggleSkill(name: string) {
    const on = skillVisible.includes(name);
    applyVis(
      mcpVisible,
      on ? skillVisible.filter((x) => x !== name) : [...skillVisible, name],
      `${on ? "公開を解除しました：" : "公開を有効にしました："} Skill「${name}」`,
    );
  }
  async function toggleTool(t: Tool) {
    const on = t.agents.includes(agentKey);
    const nextAgents = on ? t.agents.filter((k) => k !== agentKey) : [...t.agents, agentKey];
    // optimistic update
    setTools((ts) => ts.map((x) => (x.key === t.key ? { ...x, agents: nextAgents } : x)));
    try {
      await api.saveTool(t.key, {
        description: t.description,
        schema: t.schema,
        agents: nextAgents,
        enabled: t.enabled,
      });
      toast.success(`${on ? "関連付けを解除しました：" : "関連付けました："}ツール「${t.key}」`);
      onSaved?.(); // refresh the list so the card's ツール count stays in sync
    } catch (e) {
      toast.error("ツールの関連付けの保存に失敗しました：" + (e as Error).message);
      reload();
      api.tools().then(setTools).catch(() => {});
    }
  }

  if (loaded && !detail) {
    return <div className="text-muted-foreground p-6 text-center text-sm">Agentが見つかりません：{agentKey}</div>;
  }
  // config is meaningless for the conversational main agent and the fixed-budget
  // goals decomposer; every other agent (workers, custom assistants) honors it.
  const showConfig = agentKey !== "mainagent" && agentKey !== "goals";
  // web search applies to every conversational/executing agent except the one-shot
  // goals decomposer; it's gated by the global master switch.
  const showWebSearch = agentKey !== "goals";
  // interactive shell (永続 PTY セッションツール群)も goals 以外の agent に公開し、グローバルの制御はない。
  const showInteractiveShell = agentKey !== "goals";
  // 各 agent(goals/mainagent を含む)はすべて何らかの LLM で動作するため、「デフォルトモデル」のバインドは全 agent に公開する。
  const showLLM = true;
  // triggers (P3) only attach to custom agents.
  const isCustom = !!detail && !detail.agent?.builtin;

  return (
    <Tabs defaultValue="prompt" className="flex min-h-0 flex-1 flex-col">
      <TabsList className="mx-4 mt-2 w-fit">
        <TabsTrigger value="prompt">設定とプロンプト</TabsTrigger>
        <TabsTrigger value="wrapup">終了処理用プロンプト</TabsTrigger>
        <TabsTrigger value="mcp">MCP</TabsTrigger>
        <TabsTrigger value="skill">Skill</TabsTrigger>
        <TabsTrigger value="tools">Tools</TabsTrigger>
        {isCustom && <TabsTrigger value="triggers">発動条件</TabsTrigger>}
      </TabsList>

      {/* 設定 + プロンプト */}
      <TabsContent value="prompt" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <div className="grid gap-4">
          {(showLLM || showConfig || showWebSearch || showInteractiveShell) && (
            <div className="grid gap-3 rounded-md border p-3">
              {showLLM && (
                <div className="grid gap-1.5">
                  <Label htmlFor="llm-profile" className="text-xs">標準モデル（LLM 設定）</Label>
                  <div className="flex flex-wrap items-center gap-3">
                    <Select value={llmProfileId || "__follow__"} onValueChange={(v) => setLlmProfileId(v === "__follow__" ? "" : v)}>
                      <SelectTrigger id="llm-profile" className="h-8 w-72">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="__follow__">タスク / 全体の有効な設定に従う</SelectItem>
                        {llmProfiles.map((p) => (
                          <SelectItem key={p.id} value={String(p.id)}>
                            {p.name}（{p.model}）{p.is_default ? " · デフォルト" : ""}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <span className="text-muted-foreground max-w-md text-xs">
                      このAgentに固定の LLM 設定を関連付けます（「設定を保存」で反映）。優先度：Agentの設定 &gt; タスク / セッションの指定 &gt; 全体の有効な設定。
                    </span>
                  </div>
                </div>
              )}
              <div className="flex flex-wrap items-end gap-3">
                {showConfig && (
                  <>
                    <div className="grid gap-1.5">
                      <Label htmlFor="max-turns" className="text-xs">最大ループ回数（0 = 無制限）</Label>
                      <Input id="max-turns" type="number" min={0} className="h-8 w-32"
                        value={maxTurns} onChange={(e) => setMaxTurns(e.target.value)} />
                    </div>
                    <div className="grid gap-1.5">
                      <Label htmlFor="run-seconds" className="text-xs">実行時間（秒、0 = 無制限）</Label>
                      <Input id="run-seconds" type="number" min={0} className="h-8 w-32"
                        value={runSecs} onChange={(e) => setRunSecs(e.target.value)} />
                    </div>
                  </>
                )}
                <Button size="sm" variant="outline" onClick={saveConfig}>
                  <SaveIcon /> 設定を保存
                </Button>
              </div>
              {showWebSearch && (
                <div className="flex items-center gap-3 border-t pt-3">
                  <Switch
                    id="web-search"
                    checked={webSearch}
                    disabled={!webSearchGlobalOn}
                    onCheckedChange={setWebSearch}
                  />
                  <div className="grid gap-0.5">
                    <Label htmlFor="web-search" className="text-sm">ウェブ検索</Label>
                    <span className="text-muted-foreground text-xs">
                      {webSearchGlobalOn
                        ? "有効にして上の保存ボタンを押すと、このAgentが web_search でウェブ検索できます"
                        : "先に「システム設定」でウェブ検索を有効化し、検索元を設定してください"}
                    </span>
                  </div>
                </div>
              )}
              {showInteractiveShell && (
                <div className="flex items-center gap-3 border-t pt-3">
                  <Switch
                    id="interactive-shell"
                    checked={interactiveShell}
                    onCheckedChange={setInteractiveShell}
                  />
                  <div className="grid gap-0.5">
                    <Label htmlFor="interactive-shell" className="text-sm">対話型 Shell</Label>
                    <span className="text-muted-foreground text-xs">
                      この Agent で有効にして上で保存すると、永続 PTY セッションのツール（shell_open/send/read/close/list）で msfconsole / ssh / REPL などの対話型プログラムを操作できます
                    </span>
                  </div>
                </div>
              )}
            </div>
          )}

          <div className="grid gap-2">
            <Label className="text-muted-foreground text-xs">変数（クリックしてプレースホルダーを挿入。描画時に実行時データで置換）</Label>
            <div className="flex flex-wrap gap-2">
              {variables.map((v) => (
                <Tooltip key={v.name}>
                  <TooltipTrigger asChild>
                    <button type="button" onClick={() => setPrompt((p) => `${p}{{.${v.name}}}`)}
                      className="hover:bg-muted inline-flex items-center gap-1 rounded-md border bg-muted/40 px-2 py-1 font-mono text-xs">
                      {`{{.${v.name}}}`}
                      <Badge variant="secondary" className="px-1 py-0 text-[10px]">{v.source}</Badge>
                    </button>
                  </TooltipTrigger>
                  <TooltipContent className="max-w-xs">
                    <p className="font-medium">{v.description}</p>
                    <p className="text-muted-foreground mt-1">例：{v.example}</p>
                  </TooltipContent>
                </Tooltip>
              ))}
              {variables.length === 0 && <span className="text-muted-foreground text-xs">（変数なし）</span>}
            </div>
          </div>

          <Textarea className="font-mono text-xs" rows={16} value={prompt}
            placeholder="空欄で組み込みの標準プロンプトを使用" onChange={(e) => setPrompt(e.target.value)} />

          <div className="flex flex-wrap gap-2">
            <Dialog>
              <DialogTrigger asChild>
                <Button variant="outline" size="sm" onClick={doPreview}>
                  <EyeIcon /> 描画をプレビュー
                </Button>
              </DialogTrigger>
              <DialogContent className="sm:max-w-2xl">
                <DialogHeader>
                  <DialogTitle>描画プレビュー</DialogTitle>
                  <DialogDescription>すべての {`{{.Var}}`} をバックエンドでサンプル値に置き換えました。</DialogDescription>
                </DialogHeader>
                <div className="max-h-[60vh] overflow-auto rounded-md border bg-muted/30 p-3">
                  <Markdown text={preview} />
                </div>
              </DialogContent>
            </Dialog>
            <Button size="sm" onClick={savePrompt}>
              <SaveIcon /> 新しいバージョンとして保存
            </Button>
            <Button variant="outline" size="sm" onClick={resetPrompt}>
              <RotateCcwIcon /> 既定値に戻す
            </Button>
          </div>


          <Separator />
          <div className="grid gap-2">
            <Label className="text-muted-foreground text-xs">バージョン履歴</Label>
            <ul className="grid gap-1">
              {versions.map((ver, i) => (
                <li key={ver.version} className="flex items-center gap-2 rounded-md px-1 py-0.5 text-xs hover:bg-muted/50">
                  <span className="font-mono shrink-0">v{ver.version}</span>
                  {i === 0 && <Badge variant="secondary" className="px-1.5 py-0 shrink-0">現在</Badge>}
                  <span className="text-muted-foreground truncate flex-1">{ver.note}</span>
                  {ver.ts && (
                    <span className="text-muted-foreground/60 shrink-0 tabular-nums">
                      {new Date(ver.ts).toLocaleDateString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })}
                    </span>
                  )}
                  <Button variant="ghost" size="icon-sm" className="size-6 shrink-0" onClick={() => setViewVer(ver)}>
                    <EyeIcon className="size-3" />
                  </Button>
                  {i > 0 && versions[0] && (
                    <Button variant="ghost" size="icon-sm" className="size-6 shrink-0" onClick={() => setDiffVer(ver)}>
                      <GitCompareIcon className="size-3" />
                    </Button>
                  )}
                </li>
              ))}
              {versions.length === 0 && (
                <li className="text-muted-foreground text-xs">（保存済みのバージョンはありません。組み込みの既定値を使用）</li>
              )}
            </ul>
          </div>

          {/* バージョン表示 dialog */}
          <Dialog open={!!viewVer} onOpenChange={(o) => { if (!o) setViewVer(null); }}>
            <DialogContent className="sm:max-w-2xl">
              <DialogHeader>
                <DialogTitle>
                  v{viewVer?.version}
                  {viewVer?.version === versions[0]?.version && (
                    <Badge variant="secondary" className="ml-2 px-1.5 py-0 align-middle">現在</Badge>
                  )}
                </DialogTitle>
                <DialogDescription>
                  {viewVer?.note || "（備考なし）"}
                  {viewVer?.ts && (
                    <span className="ml-2 text-muted-foreground/60">
                      {new Date(viewVer.ts).toLocaleString("zh-CN")}
                    </span>
                  )}
                </DialogDescription>
              </DialogHeader>
              <pre className="bg-muted max-h-[55vh] overflow-auto whitespace-pre-wrap rounded-md p-3 font-mono text-xs">
                {viewVer?.template_text || "（空）"}
              </pre>
              <div className="flex gap-2 justify-end">
                {viewVer && viewVer.version !== versions[0]?.version && (
                  <Button variant="outline" size="sm" onClick={() => {
                    if (viewVer) { setDiffVer(viewVer); setViewVer(null); }
                  }}>
                    <GitCompareIcon className="mr-1 size-3.5" /> 現在のバージョンと比較
                  </Button>
                )}
                <Button size="sm" onClick={() => {
                  if (viewVer) { setPrompt(viewVer.template_text); setViewVer(null); toast.success(`v${viewVer.version} をエディターに読み込みました。確認後「新しいバージョンとして保存」を押してください`); }
                }}>
                  エディターに読み込む
                </Button>
              </div>
            </DialogContent>
          </Dialog>

          {/* バージョン比較 dialog */}
          <Dialog open={!!diffVer} onOpenChange={(o) => { if (!o) setDiffVer(null); }}>
            <DialogContent className="sm:max-w-3xl">
              <DialogHeader>
                <DialogTitle>バージョン比較：v{diffVer?.version} → v{versions[0]?.version}（現在）</DialogTitle>
                <DialogDescription>
                  <span className="inline-flex items-center gap-3 text-xs">
                    <span className="rounded bg-red-500/15 px-1.5 py-0.5 text-red-600 dark:text-red-400">- 削除</span>
                    <span className="rounded bg-green-500/15 px-1.5 py-0.5 text-green-600 dark:text-green-400">+ 追加</span>
                  </span>
                </DialogDescription>
              </DialogHeader>
              <DiffView oldText={diffVer?.template_text ?? ""} newText={versions[0]?.template_text ?? ""} />
            </DialogContent>
          </Dialog>
        </div>
      </TabsContent>

      {/* 終了プロンプト */}
      <TabsContent value="wrapup" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <div className="grid gap-3">
          <p className="text-muted-foreground text-xs leading-relaxed">
            このAgentが<b>タイムアウト</b>または<b>ステップ数の上限</b>で終了した場合、この「終了処理用プロンプト」で終了処理を1ラウンド実行します。検出済みで未保存の内容を書き戻し、最後に1文で要約します。空欄で組み込みの既定値を使用します。
          </p>
          <div className="flex items-center gap-2">
            <Label className="text-xs">終了処理用プロンプトの本文</Label>
            {wrapup.trim() ? (
              <Badge variant="secondary" className="px-1.5 py-0">カスタム</Badge>
            ) : (
              <Badge variant="outline" className="px-1.5 py-0">組み込みの既定値を使用</Badge>
            )}
          </div>
          <Textarea
            className="font-mono text-xs"
            rows={10}
            value={wrapup}
            placeholder={wrapupDefault || "空欄で組み込みの終了処理用プロンプトを使用"}
            onChange={(e) => setWrapup(e.target.value)}
          />
          <div className="grid gap-1.5">
            <Label htmlFor="wrapup-turns" className="text-xs">
              終了処理の最大回数（0 = 組み込みの既定値 {wrapupTurnsDefault} 回）
            </Label>
            <Input id="wrapup-turns" type="number" min={0} className="h-8 w-32"
              value={wrapupTurns} onChange={(e) => setWrapupTurns(e.target.value)} />
          </div>
          <div className="flex gap-2">
            <Button size="sm" onClick={saveWrapup}>保存</Button>
            <Button size="sm" variant="outline" onClick={resetWrapup}>既定値に戻す</Button>
          </div>
          {wrapupDefault && (
            <>
              <Separator />
              <div className="grid gap-1.5">
                <Label className="text-muted-foreground text-xs">組み込みの既定値（参照用、読み取り専用）</Label>
                <pre className="text-muted-foreground max-h-40 overflow-y-auto rounded-md border bg-muted/30 p-2 text-xs whitespace-pre-wrap">
                  {wrapupDefault}
                </pre>
              </div>
            </>
          )}

          {ttSupported && (
            <>
              <Separator className="my-2" />
              <p className="text-muted-foreground text-xs leading-relaxed">
                <b>タスクのタイムアウト時の終了処理</b>（上のper-runの終了処理とは<b>別の処理</b>）：<b>タスク全体</b>が期限に達して終了する直前に追加します。per-runの処理とは指示が逆になることがよくあります（planner の例：per-runは計画を続行、タスクの期限時は停止して最終判定）。空欄で組み込みの既定値を使用します。
              </p>
              <div className="flex items-center gap-2">
                <Label className="text-xs">タスクのタイムアウト時のプロンプト本文</Label>
                {ttWrapup.trim() ? (
                  <Badge variant="secondary" className="px-1.5 py-0">カスタム</Badge>
                ) : (
                  <Badge variant="outline" className="px-1.5 py-0">組み込みの既定値を使用</Badge>
                )}
              </div>
              <Textarea
                className="font-mono text-xs"
                rows={10}
                value={ttWrapup}
                placeholder={ttWrapupDefault || "空欄で組み込みのタスクタイムアウト用プロンプトを使用"}
                onChange={(e) => setTtWrapup(e.target.value)}
              />
              <div className="grid gap-1.5">
                <Label htmlFor="tt-turns" className="text-xs">
                  終了処理の回数（0 = 組み込みの既定値 {ttTurnsDefault} 回）
                </Label>
                <Input id="tt-turns" type="number" min={0} className="h-8 w-32"
                  value={ttTurns} onChange={(e) => setTtTurns(e.target.value)} />
              </div>
              <div className="flex gap-2">
                <Button size="sm" onClick={saveTaskTimeoutWrapup}>保存</Button>
                <Button size="sm" variant="outline" onClick={resetTaskTimeoutWrapup}>既定値に戻す</Button>
              </div>
              {ttWrapupDefault && (
                <div className="grid gap-1.5">
                  <Label className="text-muted-foreground text-xs">組み込みの既定値（参照用、読み取り専用）</Label>
                  <pre className="text-muted-foreground max-h-40 overflow-y-auto rounded-md border bg-muted/30 p-2 text-xs whitespace-pre-wrap">
                    {ttWrapupDefault}
                  </pre>
                </div>
              )}
            </>
          )}
        </div>
      </TabsContent>

      {/* MCP 可視性 */}
      <TabsContent value="mcp" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">このAgentに公開する MCP サーバーを選択してください。</p>
        <div className="grid gap-2">
          {mcp.map((m) => (
            <label key={m.id} className="flex items-center gap-2 rounded-md border p-2 text-sm">
              <Checkbox checked={mcpVisible.includes(m.id)} onCheckedChange={() => toggleMcp(m.id)} />
              {m.name}
              <span className="text-muted-foreground ml-auto text-xs">{m.transport}</span>
            </label>
          ))}
          {mcp.length === 0 && <span className="text-muted-foreground text-xs">（MCP はありません）</span>}
        </div>
      </TabsContent>

      {/* Skill 可視性 */}
      <TabsContent value="skill" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">このAgentに公開するSkillを選択してください。</p>
        <div className="grid gap-2">
          {skills.map((s) => (
            <label key={s.name} className="flex items-center gap-2 rounded-md border p-2 text-sm">
              <Checkbox checked={skillVisible.includes(s.name)} onCheckedChange={() => toggleSkill(s.name)} />
              <span className="font-mono text-xs">{s.name}</span>
              {s.description && <span className="text-muted-foreground ml-auto truncate text-xs">{s.description}</span>}
            </label>
          ))}
          {skills.length === 0 && <span className="text-muted-foreground text-xs">（Skillはありません）</span>}
        </div>
      </TabsContent>

      {/* Tools バインド */}
      <TabsContent value="tools" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">このAgentに関連付ける組み込みツールを選択してください。</p>
        <div className="grid gap-2">
          {tools.map((t) => {
            const isTraffic = TRAFFIC_TOOL_KEYS.has(t.key);
            const gated = isTraffic && !captureOn; // traffic tools need トラフィックキャプチャ on
            return (
              <label
                key={t.key}
                className={cn(
                  "flex items-center gap-2 rounded-md border p-2 text-sm",
                  gated && "opacity-60",
                )}
              >
                <Checkbox
                  checked={t.agents.includes(agentKey)}
                  disabled={gated}
                  onCheckedChange={() => toggleTool(t)}
                />
                <span className="font-mono text-xs">{t.key}</span>
                {isTraffic && (
                  <Badge variant="secondary" className="px-1 py-0 text-[9px]">トラフィック</Badge>
                )}
                {!t.enabled && (
                  <Badge variant="outline" className="text-destructive px-1 py-0 text-[9px]">無効</Badge>
                )}
                {gated ? (
                  <span className="text-muted-foreground ml-auto text-xs">トラフィックのキャプチャが必要です</span>
                ) : (
                  t.description && (
                    <span className="text-muted-foreground ml-auto line-clamp-1 max-w-[55%] text-xs">
                      {t.description}
                    </span>
                  )
                )}
              </label>
            );
          })}
          {tools.length === 0 && <span className="text-muted-foreground text-xs">（ツールはありません）</span>}
        </div>
      </TabsContent>

      {/* トリガー(P3、カスタム agent のみ) */}
      {isCustom && (
        <TabsContent value="triggers" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
          <AgentTriggersTab agentKey={agentKey} agent={detail?.agent} />
        </TabsContent>
      )}
    </Tabs>
  );
}

// ---------- Diff helpers ----------

type DiffLine = { type: "same" | "add" | "del"; text: string };

function computeDiff(oldText: string, newText: string): DiffLine[] {
  const a = oldText.split("\n");
  const b = newText.split("\n");
  const m = a.length;
  const n = b.length;
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array(n + 1).fill(0));
  for (let i = 1; i <= m; i++)
    for (let j = 1; j <= n; j++)
      dp[i][j] = a[i - 1] === b[j - 1] ? dp[i - 1][j - 1] + 1 : Math.max(dp[i - 1][j], dp[i][j - 1]);
  const result: DiffLine[] = [];
  let i = m;
  let j = n;
  while (i > 0 || j > 0) {
    if (i > 0 && j > 0 && a[i - 1] === b[j - 1]) {
      result.unshift({ type: "same", text: a[i - 1] });
      i--;
      j--;
    } else if (j > 0 && (i === 0 || dp[i][j - 1] >= dp[i - 1][j])) {
      result.unshift({ type: "add", text: b[j - 1] });
      j--;
    } else {
      result.unshift({ type: "del", text: a[i - 1] });
      i--;
    }
  }
  return result;
}

function DiffView({ oldText, newText }: { oldText: string; newText: string }) {
  const lines = React.useMemo(() => computeDiff(oldText, newText), [oldText, newText]);
  return (
    <pre className="max-h-[60vh] overflow-auto rounded-md border bg-muted/30 p-2 font-mono text-xs leading-5">
      {lines.map((l, idx) => (
        <div
          key={idx}
          className={cn(
            "whitespace-pre-wrap px-1",
            l.type === "del" && "bg-red-500/15 text-red-700 dark:text-red-400",
            l.type === "add" && "bg-green-500/15 text-green-700 dark:text-green-400",
            l.type === "same" && "text-muted-foreground",
          )}
        >
          <span className="select-none mr-1 opacity-50">{l.type === "del" ? "-" : l.type === "add" ? "+" : " "}</span>
          {l.text}
        </div>
      ))}
    </pre>
  );
}

// AgentTriggersTab manages a custom agent's P3 triggers: list + add + delete.
// Each trigger fires (定期/検出finding/目標達成/タスクタイムアウト/ツール呼び出し、複数選択可) → a new conversation runs
// in parallel with the base user message + auto context appended by the backend.
function AgentTriggersTab({ agentKey, agent }: { agentKey: string; agent?: Agent }) {
  const [triggers, setTriggers] = React.useState<AgentTrigger[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  // トリガー後の処理方針(agent ごと);初期値は agent detail から取得し、変更時に保存する。
  const [runMode, setRunMode] = React.useState<"serial" | "parallel">(agent?.trigger_run_mode ?? "serial");
  const [mergeMode, setMergeMode] = React.useState<"by_task" | "all" | "none">(agent?.trigger_merge_mode ?? "all");
  const [maxParallel, setMaxParallel] = React.useState(String(agent?.trigger_max_parallel ?? 5));
  React.useEffect(() => {
    setRunMode(agent?.trigger_run_mode ?? "serial");
    setMergeMode(agent?.trigger_merge_mode ?? "all");
    setMaxParallel(String(agent?.trigger_max_parallel ?? 5));
  }, [agent?.trigger_run_mode, agent?.trigger_merge_mode, agent?.trigger_max_parallel]);

  async function saveBehavior(patch: {
    trigger_run_mode?: "serial" | "parallel";
    trigger_merge_mode?: "by_task" | "all" | "none";
    trigger_max_parallel?: number;
  }) {
    try {
      await api.saveAgentConfig(agentKey, patch);
    } catch (e) {
      toast.error("ポリシーの保存に失敗しました：" + (e as Error).message);
    }
  }
  const [onInterval, setOnInterval] = React.useState(false);
  const [intervalSec, setIntervalSec] = React.useState("60");
  const [onFinding, setOnFinding] = React.useState(false);
  const [onGoalMet, setOnGoalMet] = React.useState(false);
  const [onTaskTimeout, setOnTaskTimeout] = React.useState(false);
  const [onToolCall, setOnToolCall] = React.useState(false);
  const [onTaskCreate, setOnTaskCreate] = React.useState(false);
  const [intervalMsg, setIntervalMsg] = React.useState("");
  const [findingMsg, setFindingMsg] = React.useState("");
  const [goalMsg, setGoalMsg] = React.useState("");
  const [taskTimeoutMsg, setTaskTimeoutMsg] = React.useState("");
  const [toolCallMsg, setToolCallMsg] = React.useState("");
  const [taskCreateMsg, setTaskCreateMsg] = React.useState("");
  const [toolNames, setToolNames] = React.useState<string[]>([]);
  const [saving, setSaving] = React.useState(false);
  // null = 新規追加モード；非 null = その id のトリガーを編集中。
  const [editingId, setEditingId] = React.useState<number | null>(null);

  const reload = React.useCallback(() => {
    api.agentTriggers(agentKey).then(setTriggers).catch(() => setTriggers([]));
  }, [agentKey]);
  React.useEffect(() => {
    reload();
  }, [reload]);
  React.useEffect(() => {
    api.tools().then(setTools).catch(() => setTools([]));
  }, []);

  function toggleTool(key: string) {
    setToolNames((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
  }

  // resetForm はフォームを消去して「新規追加」モードへ戻す。
  function resetForm() {
    setEditingId(null);
    setOnInterval(false);
    setIntervalSec("60");
    setOnFinding(false);
    setOnGoalMet(false);
    setOnTaskTimeout(false);
    setOnToolCall(false);
    setOnTaskCreate(false);
    setIntervalMsg("");
    setFindingMsg("");
    setGoalMsg("");
    setTaskTimeoutMsg("");
    setToolCallMsg("");
    setTaskCreateMsg("");
    setToolNames([]);
  }

  // startEdit は既存トリガーをフォームへ入れ、「編集」モードにする。
  function startEdit(t: AgentTrigger) {
    setEditingId(t.id);
    setOnInterval(t.interval_sec > 0);
    setIntervalSec(t.interval_sec > 0 ? String(t.interval_sec) : "60");
    setOnFinding(t.on_finding);
    setOnGoalMet(t.on_goal_met);
    setOnTaskTimeout(t.on_task_timeout);
    setOnToolCall(t.on_tool_call);
    setOnTaskCreate(t.on_task_create);
    setIntervalMsg(t.interval_message);
    setFindingMsg(t.finding_message);
    setGoalMsg(t.goal_message);
    setTaskTimeoutMsg(t.task_timeout_message);
    setToolCallMsg(t.tool_call_message);
    setTaskCreateMsg(t.task_create_message);
    setToolNames(t.tool_names ?? []);
  }

  // submit は editingId に応じて「追加」か「変更を保存」を行う;編集時はそのトリガーの有効状態を保持する。
  async function submit() {
    const n = onInterval ? Math.max(1, Math.floor(Number(intervalSec) || 0)) : 0;
    if (n === 0 && !onFinding && !onGoalMet && !onTaskTimeout && !onToolCall && !onTaskCreate) {
      toast.error("トリガー条件を1つ以上選択してください");
      return;
    }
    if (onToolCall && toolNames.length === 0) {
      toast.error("ツール呼び出しのトリガーには、ツールを1つ以上選択してください");
      return;
    }
    const body = {
      interval_sec: n,
      on_finding: onFinding,
      on_goal_met: onGoalMet,
      on_task_timeout: onTaskTimeout,
      on_tool_call: onToolCall,
      on_task_create: onTaskCreate,
      interval_message: intervalMsg.trim(),
      finding_message: findingMsg.trim(),
      goal_message: goalMsg.trim(),
      task_timeout_message: taskTimeoutMsg.trim(),
      tool_call_message: toolCallMsg.trim(),
      task_create_message: taskCreateMsg.trim(),
      tool_names: onToolCall ? toolNames : [],
    };
    setSaving(true);
    try {
      if (editingId != null) {
        const cur = triggers.find((x) => x.id === editingId);
        await api.updateTrigger(editingId, { ...body, enabled: cur?.enabled ?? true });
        toast.success("変更を保存しました");
      } else {
        await api.createTrigger(agentKey, { ...body, enabled: true });
        toast.success("トリガーを追加しました");
      }
      resetForm();
      reload();
    } catch (e) {
      toast.error((editingId != null ? "保存に失敗しました：" : "追加に失敗しました：") + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }
  async function toggleEnabled(t: AgentTrigger) {
    try {
      await api.updateTrigger(t.id, {
        enabled: !t.enabled,
        interval_sec: t.interval_sec,
        on_finding: t.on_finding,
        on_goal_met: t.on_goal_met,
        on_task_timeout: t.on_task_timeout,
        on_tool_call: t.on_tool_call,
        on_task_create: t.on_task_create,
        interval_message: t.interval_message,
        finding_message: t.finding_message,
        goal_message: t.goal_message,
        task_timeout_message: t.task_timeout_message,
        tool_call_message: t.tool_call_message,
        task_create_message: t.task_create_message,
        tool_names: t.tool_names,
      });
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function del(id: number) {
    try {
      await api.deleteTrigger(id);
      if (editingId === id) resetForm();
      reload();
    } catch (e) {
      toast.error("削除に失敗しました：" + (e as Error).message);
    }
  }

  function condLabel(t: AgentTrigger): string {
    const parts: string[] = [];
    if (t.interval_sec > 0) parts.push(`間隔： ${t.interval_sec}s`);
    if (t.on_finding) parts.push("検出結果 finding の発生");
    if (t.on_goal_met) parts.push("目標の達成");
    if (t.on_task_timeout) parts.push("タスクのタイムアウト");
    if (t.on_tool_call) parts.push(`ツール呼び出し（${t.tool_names.length} 件）`);
    if (t.on_task_create) parts.push("タスクの作成");
    return parts.join(" · ") || "（条件なし）";
  }

  return (
    <div className="grid gap-4">
      <p className="text-muted-foreground text-xs">
        トリガーで、このカスタム Agent を自動実行できます。発動するたびに<b>新しいチャットを作成</b>します（「チャット」画面に表示）。条件は複数選択可能です。基本メッセージの後に、発動理由と関連するタスク / finding / 目標を自動で追加します。
      </p>

      {/* トリガー後の処理方針 */}
      <div className="grid gap-3 rounded-md border p-3">
        <Label className="text-muted-foreground text-xs">発動後の処理ポリシー（キューと実行の統合方法）</Label>
        <div className="flex flex-wrap items-center gap-4">
          <div className="grid gap-1">
            <Label className="text-xs">実行モード</Label>
            <Select
              value={runMode}
              onValueChange={(v) => {
                const rm = v as "serial" | "parallel";
                setRunMode(rm);
                saveBehavior({ trigger_run_mode: rm });
              }}
            >
              <SelectTrigger size="sm" className="h-8 w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent position="popper">
                <SelectItem value="serial">直列（待機し、1件ずつ実行）</SelectItem>
                <SelectItem value="parallel">並列（各セッションを同時実行）</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="grid gap-1">
            <Label className="text-xs">統合モード</Label>
            <Select
              value={mergeMode}
              disabled={runMode === "parallel"}
              onValueChange={(v) => {
                const mm = v as "by_task" | "all" | "none";
                setMergeMode(mm);
                saveBehavior({ trigger_merge_mode: mm });
              }}
            >
              <SelectTrigger size="sm" className="h-8 w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent position="popper">
                <SelectItem value="by_task">タスクごとに統合</SelectItem>
                <SelectItem value="all">すべて1件に統合</SelectItem>
                <SelectItem value="none">統合しない</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {runMode === "parallel" && (
            <div className="grid gap-1">
              <Label htmlFor="tr-maxpar" className="text-xs">最大同時実行数（0 = 無制限）</Label>
              <Input
                id="tr-maxpar"
                type="number"
                min={0}
                className="h-8 w-28"
                value={maxParallel}
                onChange={(e) => setMaxParallel(e.target.value)}
                onBlur={() => {
                  const n = Math.max(0, Math.floor(Number(maxParallel) || 0));
                  setMaxParallel(String(n));
                  saveBehavior({ trigger_max_parallel: n });
                }}
              />
            </div>
          )}
        </div>
        <p className="text-muted-foreground text-xs">
          {runMode === "parallel"
            ? "並列：発動ごとに独立したセッションを同時実行し、統合しません。上限を超えた分は空きができるまで待機します。"
            : mergeMode === "by_task"
              ? "直列・タスクごとに統合：同じagentは1件ずつ実行し、待機中の同じタスクのイベントを1つのセッションに統合します。"
              : mergeMode === "all"
                ? "直列・すべて統合：同じagentは1件ずつ実行し、取り出す時点のすべての待機トリガーを1つのセッションに統合します。"
                : "直列・統合なし：同じagentは1件ずつ実行し、発動ごとに独立したセッションを使用します。"}
        </p>
      </div>

      {/* トリガーの追加 / 編集 */}
      <div className="grid gap-3 rounded-md border p-3">
        <Label className="text-muted-foreground text-xs">
          {editingId != null
            ? `トリガー #${editingId} を編集（編集後に「変更を保存」を押してください）`
            : "トリガーを追加（条件ごとにユーザーメッセージを指定可能）"}
        </Label>

        {/* 定期 */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onInterval} onCheckedChange={(v) => setOnInterval(!!v)} /> 定期的に実行
          </label>
          {onInterval && (
            <div className="grid gap-1.5">
              <div className="flex items-center gap-2">
                <Label htmlFor="tr-interval" className="text-xs">間隔：</Label>
                <Input id="tr-interval" type="number" min={1} className="h-8 w-24"
                  value={intervalSec} onChange={(e) => setIntervalSec(e.target.value)} />
                <span className="text-muted-foreground text-xs">秒</span>
              </div>
              <Textarea className="text-xs" rows={2} value={intervalMsg}
                placeholder="定期実行時に agent に送るメッセージ。例：すべてのタスクを点検" onChange={(e) => setIntervalMsg(e.target.value)} />
            </div>
          )}
        </div>

        {/* finding */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onFinding} onCheckedChange={(v) => setOnFinding(!!v)} /> finding の検出時に実行
          </label>
          {onFinding && (
            <Textarea className="text-xs" rows={2} value={findingMsg}
              placeholder="finding の検出時に agent に送るメッセージ（タスクと finding の詳細を自動追加）" onChange={(e) => setFindingMsg(e.target.value)} />
          )}
        </div>

        {/* 目標達成 */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onGoalMet} onCheckedChange={(v) => setOnGoalMet(!!v)} /> 目標の達成時に実行
          </label>
          {onGoalMet && (
            <Textarea className="text-xs" rows={2} value={goalMsg}
              placeholder="目標達成時に agent に送るメッセージ（タスクと達成した目標を自動追加）" onChange={(e) => setGoalMsg(e.target.value)} />
          )}
        </div>

        {/* タスクタイムアウト */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onTaskTimeout} onCheckedChange={(v) => setOnTaskTimeout(!!v)} /> タスクのタイムアウト時に実行
          </label>
          {onTaskTimeout && (
            <Textarea className="text-xs" rows={2} value={taskTimeoutMsg}
              placeholder="タイムアウト時に agent に送るメッセージ（タスク番号と目標を自動追加）" onChange={(e) => setTaskTimeoutMsg(e.target.value)} />
          )}
        </div>

        {/* ツール呼び出し */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onToolCall} onCheckedChange={(v) => setOnToolCall(!!v)} /> ツール呼び出し時に実行
          </label>
          {onToolCall && (
            <div className="grid gap-1.5">
              <div className="text-muted-foreground text-xs">
                監視するツールを1つ以上選択してください。タスク実行中にこれらのツールの<b>呼び出しが完了</b>するたびに発動します。選択済み： {toolNames.length} 個。
              </div>
              <div className="max-h-40 overflow-y-auto rounded-md border p-2">
                {tools.length === 0 && <span className="text-muted-foreground text-xs">（ツール一覧は空です）</span>}
                <div className="grid gap-1">
                  {tools.map((tool) => (
                    <label key={tool.key} className="flex items-start gap-2 text-xs">
                      <Checkbox className="mt-0.5" checked={toolNames.includes(tool.key)}
                        onCheckedChange={() => toggleTool(tool.key)} />
                      <span className="min-w-0">
                        <span className="font-medium">{tool.key}</span>
                        {tool.description && <span className="text-muted-foreground line-clamp-1"> {tool.description}</span>}
                      </span>
                    </label>
                  ))}
                </div>
              </div>
              <Textarea className="text-xs" rows={2} value={toolCallMsg}
                placeholder="ツール呼び出し時に agent に送るメッセージ（タスク情報、入力パラメーター、戻り値を自動追加）" onChange={(e) => setToolCallMsg(e.target.value)} />
            </div>
          )}
        </div>

        {/* タスク作成 */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onTaskCreate} onCheckedChange={(v) => setOnTaskCreate(!!v)} /> タスクの作成時に実行
          </label>
          {onTaskCreate && (
            <Textarea className="text-xs" rows={2} value={taskCreateMsg}
              placeholder="タスク作成時に agent に送るメッセージ（タスク番号と目標を自動追加）" onChange={(e) => setTaskCreateMsg(e.target.value)} />
          )}
        </div>

        <div className="flex items-center gap-2">
          <Button size="sm" onClick={submit} disabled={saving}>
            <SaveIcon /> {editingId != null ? "変更を保存" : "トリガーを追加"}
          </Button>
          {editingId != null && (
            <Button size="sm" variant="ghost" onClick={resetForm} disabled={saving}>
              <XIcon /> 編集をキャンセル
            </Button>
          )}
        </div>
      </div>

      {/* 既存トリガー */}
      <div className="grid gap-2">
        <Label className="text-muted-foreground text-xs">既存のトリガー</Label>
        {triggers.length === 0 && <span className="text-muted-foreground text-xs">（なし）</span>}
        {triggers.map((t) => (
          <div
            key={t.id}
            className={cn(
              "flex items-start gap-2 rounded-md border p-2 text-sm",
              editingId === t.id && "border-primary bg-primary/5",
            )}
          >
            <Switch checked={t.enabled} onCheckedChange={() => toggleEnabled(t)} className="mt-0.5" />
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="font-medium">{condLabel(t)}</span>
                {!t.enabled && (
                  <Badge variant="outline" className="text-destructive px-1 py-0 text-[9px]">無効</Badge>
                )}
              </div>
              <div className="text-muted-foreground grid gap-0.5 text-xs">
                {t.interval_sec > 0 && t.interval_message && <div className="line-clamp-1">定期：{t.interval_message}</div>}
                {t.on_finding && t.finding_message && <div className="line-clamp-1">finding：{t.finding_message}</div>}
                {t.on_goal_met && t.goal_message && <div className="line-clamp-1">目標：{t.goal_message}</div>}
                {t.on_task_timeout && t.task_timeout_message && <div className="line-clamp-1">タイムアウト：{t.task_timeout_message}</div>}
                {t.on_task_create && t.task_create_message && <div className="line-clamp-1">タスク作成：{t.task_create_message}</div>}
                {t.on_tool_call && (
                  <>
                    <div className="line-clamp-1">ツール：{t.tool_names.join("、") || "（未選択）"}</div>
                    {t.tool_call_message && <div className="line-clamp-1">メッセージ：{t.tool_call_message}</div>}
                  </>
                )}
              </div>
            </div>
            <Button variant="ghost" size="icon-sm" className="text-muted-foreground hover:text-foreground"
              onClick={() => startEdit(t)} title="編集">
              <PencilIcon className="size-3.5" />
            </Button>
            <Button variant="ghost" size="icon-sm" className="text-muted-foreground hover:text-destructive"
              onClick={() => del(t.id)} title="削除">
              <Trash2Icon className="size-3.5" />
            </Button>
          </div>
        ))}
      </div>
    </div>
  );
}
