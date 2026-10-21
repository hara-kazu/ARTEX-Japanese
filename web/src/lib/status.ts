// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: 意图 / 覆盖 / 任务 / 严重度 each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose 用作「严重」——实心高强调,视觉上明显高于「高危」的软红描边。
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "割り当て待ち", tone: "slate" },
  running: { label: "実行中", tone: "blue" },
  paused: { label: "一時停止中", tone: "amber" },
  done: { label: "完了", tone: "green" },
  // blocked = 模型/API/网络故障重试用尽，这条意图基本没真正探成（非目标拦截）。
  blocked: { label: "実行エラー", tone: "red" },
  // exhausted = 达到步数/时间预算被中途掐断、只写回部分结果（非方向已探尽）。
  exhausted: { label: "予算切れ", tone: "violet" },
  // stopped = 历史软删除状态（保留,历史数据）。
  stopped: { label: "停止済み", tone: "slate" },
  // deleted = 用户假删除了该意图（保留节点与血缘，删除原因见 delete_reason 字段）。
  deleted: { label: "削除済み", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "作成済み", tone: "slate" },
  queued: { label: "待機中", tone: "amber" },
  running: { label: "実行中", tone: "blue" },
  paused: { label: "一時停止中", tone: "amber" },
  done: { label: "完了", tone: "green" },
  failed: { label: "失敗", tone: "red" },
  timeout: { label: "タイムアウト", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "緊急", tone: "rose" },
  high: { label: "高", tone: "red" },
  medium: { label: "中", tone: "amber" },
  low: { label: "低", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "未処理", tone: "amber" },
  in_progress: { label: "処理中", tone: "blue" },
  confirmed: { label: "確認済み", tone: "red" },
  resolved: { label: "処理済み", tone: "green" },
  fixed: { label: "修正済み", tone: "green" },
  false_positive: { label: "誤検知", tone: "slate" },
  ignored: { label: "無視", tone: "neutral" },
  duplicate: { label: "重複", tone: "neutral" },
  risk_accepted: { label: "リスク受容", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "探索中", tone: "blue" },
  paused: { label: "一時停止中", tone: "amber" },
  stalled: { label: "停滞", tone: "red" },
  idle: { label: "待機中", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "進行中", tone: "blue" },
  met: { label: "達成済み", tone: "green" },
  abandoned: { label: "中止", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "許可", tone: "green" },
  block: { label: "ブロック", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "観測", tone: "slate" },
  confirmed: { label: "確認", tone: "green" },
  tombstoned: { label: "廃止", tone: "neutral" },
};

// 推送投递状态。sending 用 blue 而不是 amber：它不是「有问题」，
// 而是「已被领取、正在发」，与 pending 的等待语义要能区分开。
const delivery: Record<string, StatusMeta> = {
  pending: { label: "送信待ち", tone: "amber" },
  sending: { label: "送信中", tone: "blue" },
  sent: { label: "配信済み", tone: "green" },
  failed: { label: "失敗", tone: "red" },
  skipped: { label: "スキップ済み", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}
