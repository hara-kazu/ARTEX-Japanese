import type { NewAssetType } from "@/lib/types";

const ASSET_TYPE_LABELS: Record<NewAssetType, string> = {
  app: "アプリ",
  endpoint: "API",
  ip: "IP",
  root_domain: "ルートドメイン",
  service: "サービス",
  subdomain: "サブドメイン",
};

const TASK_ASSET_SOURCE_LABELS: Record<string, string> = {
  agent: "Agentによる検出",
  anchor: "共有ボードのアンカー",
  api: "資産 API",
  company: "企業の関連付け",
  legacy: "過去の関連付け",
  manual: "手動追加",
  system: "システムの関連付け",
  task: "タスクの初期設定",
};

export function taskAssetTypeLabel(type: NewAssetType): string {
  return ASSET_TYPE_LABELS[type];
}

export function taskAssetSourceLabel(source: string): string {
  return TASK_ASSET_SOURCE_LABELS[source] ?? source;
}
