// ARTEX domain model — types used across the UI.
// Derived from the functional spec (section 7: 主要データ構造).

export type TaskStatus = "created" | "queued" | "running" | "paused" | "done" | "failed" | "timeout";
export type EngineMode = "exploring" | "paused" | "stalled" | "idle";

export interface Task {
  id: string;
  name?: string; // 任意のタスク名;空/省略=無名。表示時は説明にフォールバックする
  category_id?: number;
  category_name?: string;
  pinned?: boolean;
  pinned_at?: string | null;
  description: string;
  goal: string;
  status: TaskStatus;
  created_at: string;
  created_unix?: number; // created_at as unix seconds (run-duration calc)
  completed_at?: string; // RFC3339 finish time (done/failed); "" if unfinished
  completed_unix?: number; // completed_at as unix seconds (0/undef if unfinished)
  last_activity_unix?: number; // unix seconds of the last activity (0/undef if none)
  paused?: boolean;
  queued?: boolean;
  active?: boolean;
  in_flight?: number;
  findings?: { critical: number; high: number; medium: number; low: number }; // 登録済み脆弱性数(重大度別)
  last_activity?: string;
  stalled?: boolean;
  goals_total?: number;
  goals_met?: number;
  engine_mode?: EngineMode;
  tokens?: TokenTotal; // whole-task token consumption
  llm_profile_id?: number; // LLM profile used; absent = default profile
  llm_profile_ids?: number[]; // ordered task-level failover chain
  active_llm_profile_id?: number; // profile used by the next LLM call
  llm_failover_state?: "default" | "ready" | "chain_exhausted" | string;
  llm_failover_reason?: string;
  source_task_ids?: string[]; // directly related tasks inherited as read-only context
  archive_blocked_by_task_id?: string; // live direct dependent that must be archived first
  company_ids?: number[]; // associated company scopes; current company assets join the task at creation
  coverage_enabled?: boolean; // 資産カバレッジ機能スイッチ(作成時に決定、デフォルトは有効)；false=カバレッジを計算/表示しない
}

export interface TaskCategory {
  id: number;
  name: string;
  task_count: number;
  created_at: string;
  updated_at: string;
}

export interface TaskTemplate {
  id: number;
  name: string;
  description: string;
  goal: string;
  category_id?: number | null; // 初期分類；null/省略=なし
  intercept_rules?: AssetInterceptRuleInput[]; // 初期のタスク単位インターセプト/許可ルール
  created_at: string;
  updated_at: string;
}

export interface DeleteTaskOptions {
  delete_assets: boolean;
  delete_traffic: boolean;
  delete_files: boolean;
  delete_findings: boolean;
  delete_llm_records: boolean;
}

export interface DeleteTaskResult {
  deleted: string;
  assets_deleted: number;
  assets_detached: number;
  traffic_deleted: number;
  files_deleted: boolean;
  findings_deleted: number;
  llm_records_deleted: number;
  cleanup_warning?: string;
}

export type TaskArchiveState =
  | "archive_queued"
  | "archiving"
  | "archive_failed"
  | "ready"
  | "restore_queued"
  | "restoring"
  | "restore_failed"
  | "delete_queued"
  | "deleting"
  | "delete_failed";

export interface TaskArchiveTokenStats {
  calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface TaskArchive {
  id: number;
  task_id: number;
  state: TaskArchiveState;
  phase: string;
  progress: number;
  error?: string;
  warnings?: string[];
  format_version: number;
  sha256?: string;
  original_size: number;
  compressed_size: number;
  task_name: string;
  task_description: string;
  task_goal: string;
  original_status: TaskStatus;
  category_id?: number;
  category_name?: string;
  source_task_ids: number[];
  remaining_timeout_seconds: number;
  data_counts: Record<string, number>;
  aggregate_stats: {
    tokens?: TaskArchiveTokenStats;
    skills?: Record<string, number>;
    tools?: Record<string, number>;
    findings?: Record<string, number>;
  };
  archived_at?: string;
  requested_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskArchivePage {
  items: TaskArchive[];
  total: number;
  page: number;
  size: number;
}

export interface ArchiveBatchItem {
  id: string;
  archive_id?: number;
  ok: boolean;
  queued: boolean;
  error?: string;
}

// ---- Asset graph (global, shared across tasks) ----
export type AssetType =
  | "company"
  | "domain"
  | "ip"
  | "port"
  | "service"
  | "site"
  | "endpoint"
  | "parameter"
  | "tech"
  | "credential"
  | "data";

export type NodeState = "observed" | "confirmed" | "tombstoned";

export interface AssetNode {
  id: string;
  type: AssetType;
  name: string;
  key: string; // nkey
  value?: string;
  company_id?: string; // 所属企業の資産 id；空=未所属
  state: NodeState;
  confidence: number; // 0..1
  attrs?: Record<string, unknown>;
  first_seen: string;
  last_seen: string;
}

export type AssetRel =
  | "owns"
  | "resolves"
  | "exposes"
  | "runs"
  | "serves"
  | "has_endpoint"
  | "has_param"
  | "fingerprinted"
  | "authenticates_as"
  | "reachable"
  | "has_subdomain";

export interface Edge {
  src: string;
  dst: string;
  rel: AssetRel | ExploreRel;
}

// Task asset view — server-side enriched, paginated.
export interface TaskAssetRef {
  id: string;
  name?: string;
  key: string;
  attrs?: Record<string, unknown>;
}

export interface TaskAssetItem extends AssetNode {
  techs?: TaskAssetRef[];
  auth?: TaskAssetRef[];
  params?: TaskAssetRef[];
}

export interface TaskAssetView {
  counts: Record<string, number>;
  total: number;
  items: TaskAssetItem[];
}

// ---- New unified asset model (new backend) ----
export type NewAssetType = "root_domain" | "ip" | "subdomain" | "app" | "service" | "endpoint";

export interface Asset {
  id: number;
  type: NewAssetType;
  company_id?: number;
  task_ids: number[];
  domain?: string;
  root_domain?: string;
  ip?: string;
  c_segment?: string;
  port?: number;
  icp?: string;
  bound_domains?: string[];
  open_ports?: { port: number; service?: string }[];
  record_type?: string;
  record_value?: string[] | string;
  bundle_id?: string;
  app_name?: string;
  category?: string;
  app_description?: string;
  app_icp?: string;
  url?: string;
  service_type?: string;
  service_name?: string;
  favicon_mmh3?: string;
  status_code?: number;
  content_length?: number;
  page_title?: string;
  technologies?: string[];
  auth?: Record<string, unknown>[];
  method?: string;
  params?: Record<string, unknown>[];
  extra?: Record<string, unknown>;
  last_seen: string;
  task_source?: string;
  task_source_summary?: string;
  task_source_node_id?: number;
}

export interface IntentAsset {
  intent_id: number | string;
  asset_id: number;
  type: NewAssetType;
  label: string;
  source: string;
  source_summary: string;
  source_node_id?: number;
  source_task_id: number;
  inherited: boolean;
}

export interface TaskAssetMutation {
  requested: number;
  attached: number;
  existing: number;
}

export interface TaskAssetScopeMutation {
  requested: number;
  assets_linked: number;
  assets_existing: number;
  scopes_added: number;
  scopes_existing: number;
}

// ---- Asset coverage graph (per task) ----
// 力指向「資産カバレッジグラフ」のノード。key は一意：資産="a:<id>"、企業="c:<id>"、
// 資産行がないルートドメイン="r:<domain>"。in_scope=false は接続専用の灰色コンテキストノード。
export interface CoverageGraphNode {
  key: string;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint";
  label: string;
  tested: boolean;
  in_scope: boolean;
  asset_id?: number;
  company_id?: number;
  domain?: string;
  root_domain?: string;
  ip?: string;
  url?: string;
  port?: number;
  service_type?: string;
  app_name?: string;
  page_title?: string;
  status_code?: number;
}

export interface CoverageGraphEdge {
  src: string;
  dst: string;
}

export interface CoverageGraphData {
  nodes: CoverageGraphNode[];
  edges: CoverageGraphEdge[];
}

// 資産がこのタスクの探索グラフで関連付けられたインテント/事実/検出結果（カバレッジグラフのノードドロワー用）。
export interface CoverageAssetRef {
  id: number;
  kind: string;
  state: string;
  summary: string;
  source_task_id?: string;
  inherited?: boolean;
}
export interface CoverageAssetRefs {
  intents: CoverageAssetRef[];
  facts: CoverageAssetRef[];
  findings: CoverageAssetRef[];
}

// ---- Workspace file manager (workDir) ----
export interface WorkspaceEntry {
  name: string;
  path: string; // workspace-relative, forward slashes
  dir: boolean;
  size: number;
  mtime: number; // unix millis
}
export interface WorkspaceListing {
  path: string;
  entries: WorkspaceEntry[];
}
export interface WorkspaceFile {
  path: string;
  size: number;
  binary: boolean;
  too_large?: boolean;
  content?: string;
}

// タスクのテスト範囲1件（カバレッジの分母 + 許可境界）。
export interface TaskScopeRow {
  id: number;
  task_id: number;
  kind: "company" | "root_domain" | "subdomain" | "ip" | "cidr" | "icp" | "keyword";
  company_id?: number;
  company_name?: string; // バックエンドで JOIN companies により解決し、kind=company のみ値を持つ
  domain?: string;
  net?: string;
  value?: string;
  source: "auto" | "agent" | "manual";
  reason?: string;
}

export type CompanyScopeKind = "domain" | "ip" | "cidr" | "icp" | "keyword";

// 企業の新規追加時に送信する構造化した資産範囲ルール。
export interface CompanyScopeRule {
  kind: CompanyScopeKind;
  value: string;
}

// 資産範囲の書き込み結果。errors は今回送信した不正な行；warnings は今回の送信とは無関係だが、
// 所属結果を想定外にする既存データの問題（ip フィールドにホスト名を保存した資産など）。
export interface CompanyScopeMutation {
  added: number;
  skipped: number;
  invalid: number;
  errors?: string[];
  warnings?: string[];
}

// 企業の資産範囲ルール1件（所属の唯一の信頼できる情報源）。
export interface ScopeRow {
  id: number;
  company_id: number;
  kind: CompanyScopeKind;
  domain?: string; // kind=domain の場合に値を持つ
  net?: string; // kind=ip|cidr の場合に値を持つ
  value?: string; // kind=icp|keyword の場合はバックエンドから直接返る場合がある
  raw: string; // 元のユーザー入力。表示と補充用
  reason?: string;
}

// 企業：type=company の資産ノード + アイコン + 資産数 + 資産範囲ルール。
export interface Company {
  id: number;
  name: string;
  logo?: string; // リモートアイコン URL；空ならフロントエンドで名前の先頭文字を使う
  asset_count: number;
  scope?: ScopeRow[];
}

// ---- Exploration graph (per task) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";
export type GoalState = "open" | "met" | "abandoned";
export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";
export type FindingState = "confirmed" | "dismissed";
export type HintState = "active" | "consumed";
export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // インテントの論理削除(state='deleted')時の削除理由
}

// 通知ボード1ページ:作成順にページ分割したノード + このページに関係するエッジ + エッジの他端のノード(refs、id で索引)、
// これによりグラフ全体を取得せず、各通知が「どこから来て何を出力したか」を示せる。
export interface ExplorationNodePage {
  items: TaskNode[];
  total: number;
  page: number;
  size: number;
  edges: Edge[];
  refs: Record<string, TaskNode>;
  // ノード id → そのノードに紐付く資産(通知ボード展開時に一緒に表示し、このページのノードと近傍を含む)。
  assets: Record<string, FindingAsset[]>;
}

export interface ExplorationNodeQuery {
  page?: number;
  size?: number;
  kinds?: ExploreKind[];
  states?: string[];
  q?: string;
  order?: "asc" | "desc";
}

// 目標管理カードで使う目標(バックエンドで payload を text/vulnclass に分解済み)。
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 制約管理カードで使う操作制約(allow=許可 / deny=禁止)。
export type ConstraintKind = "allow" | "deny";
export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}

// ---- Findings ----
export type Severity = "critical" | "high" | "medium" | "low";

// 脆弱性の対処状態:未処理 / 処理中 / 確認済み / 処理済み / 修正済み / 誤検知 / 無視 / 重複 / リスク受容。
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset は脆弱性に紐付いた資産(バックエンドで label を事前生成済み)。
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 独立した findings テーブルの行 id。状態更新のハンドル(タスク内の古いノードは欠ける場合がある)
  vulnclass: string;
  name?: string; // 脆弱性名;空の場合の表示は vulnclass にフォールバックする
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 詳細レポート(Markdown);詳細インターフェースのみ返し、一覧では空
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage は検出結果一覧のサーバー側ページ分割レスポンス。
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 任意のタスク名;空/省略=無名
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats は検出結果の全表集約(統計カード + 脆弱性の種類ドロップダウン)で、サーバーで計算し、ページ分割に影響されない。
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption は検出結果ページの「タスク別」フィルタードロップダウンの項目:脆弱性のあるタスク(説明が空ならタスク削除済みで、
// フロントエンドは id の表示にフォールバック)と、その脆弱性件数。
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 任意のタスク名;空/省略=無名
  description: string;
  count: number;
}

// FindingQuery は検出結果一覧のページ分割/フィルター/ソートパラメータ。
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // タスク id;"all"/空 = タスクでフィルターしない
  query?: string;
  sort?: "severity" | "time";
  // 資産ツリーノード key;1ノードの選択 = そのサブツリー全体を選択。空 = 資産でフィルターしない。
  assetScope?: string;
}

// ---- Findings by asset (資産ビュー) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode は資産ツリーのノード。key は a:<id>(資産)、c:<id>(企業)、
// r:<domain>(保存先に資産行のないルートドメイン)、__none__(未関連資産)のような形。
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // その資産に直接紐付く検出結果数
  total: number; // 子孫を含み、検出結果単位で重複排除
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET はバックエンドの db.FindingUnassignedAsset に対応する。
export const FINDING_UNASSIGNED_ASSET = "__none__";

// ---- Activity / sessions ----
export type ActivityKind =
  | "tool_use"
  | "tool_result"
  | "text"
  | "thinking"
  | "result"
  | "user"
  | "intent" // LLM-generated exploration objective leading a worker session (UI-synthesized)
  | "round" // planner round boundary marker (engine-emitted)
  | "usage" // live cumulative token usage (per model turn); not rendered
  | "llm_switch" // automatic/manual task-level LLM switch
  | "llm_failover" // task-level provider switch / chain exhaustion audit event
  | "intercept_request"; // user-approval request from the intercept layer

// ChatAttachment は1回のアップロードファイル:path はそのセッション/タスク作業ディレクトリ(agent の CWD)からの相対パス。
export interface ChatAttachment {
  name: string;
  path: string;
  size: number;
  abs?: string; // 絶対パス(scope=staging の一時アップロード時に返し、タスク作成前に説明へ書き込む)
}

export interface Activity {
  seq: number;
  intent_id?: string;
  worker: string; // session owner: planner | mainagent | work#1 ...
  ts: string;
  kind: ActivityKind;
  tool?: string;
  tool_use_id?: string;
  is_error?: boolean;
  summary: string;
  detail?: string;
  metadata?: {
    llm_transition?: LLMTransition;
  };
  source_task_id?: string;
  inherited?: boolean;
  main_seg?: number; // main-agent conversation segment (present only on worker="mainagent" rows)
  // token usage (present only on kind='result')
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
}

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- Agent triggers (P3 スケジューリング、カスタム agent のみ) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 定期:毎 N 秒(0=定期実行しない)
  on_finding: boolean; // 任意のタスクで finding を検出したとき発火
  on_goal_met: boolean; // 任意のタスクが目標を達成したとき発火
  on_task_timeout: boolean; // 任意のタスクがタイムアウトしたとき発火
  on_tool_call: boolean; // 選択したツールの呼び出し(実行完了)時に発火
  on_task_create: boolean; // 任意のタスクの作成時に発火
  interval_message: string; // 各トリガー条件の個別ユーザーメッセージ
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call で選択したツール key(1つ以上)
  last_fire?: string;
}

// ---- Conversations (chat page) ----
export interface ActiveFindingRetest {
  id: number;
  finding_id: string;
  conversation_id: number;
  status: "pending" | "running";
}

export interface FindingRetest {
  id: number;
  finding_id: number;
  conversation_id: number | null;
  status: "pending" | "running" | "completed" | "failed" | "stopped";
  verdict: "" | "reproduced" | "fixed" | "inconclusive";
  notes: string;
  summary: string;
  evidence: string;
  error: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Conversation {
  id: number;
  running?: boolean; // live server state, returned with the conversation list
  agent_key: string;
  title: string;
  llm_profile_id?: number;
  pinned?: boolean;
  pinned_at?: string | null;
  created_at: string;
  updated_at: string;
}

// ---- Backend logs (/logs page) ----
export interface LogLine {
  seq: number;
  db_id?: number; // server_logs.id; present for DB-persisted lines
  ts: string;
  level: "info" | "warn" | "error";
  tag: string;
  text: string;
}

export type SessionRole = "mainagent" | "planner" | "worker" | "system";
export type SessionStatus = "running" | "paused" | "done" | "blocked" | "exhausted" | "pending" | "stopped" | "deleted";

// Daily token aggregate bucket (GET /api/tokens/daily).
export interface DailyTokenBucket {
  date: string; // "YYYY-MM-DD"
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Per-worker token usage (GET /api/exploration/tokens).
export interface TokenUsage {
  worker: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface SessionTokenUsage {
  session: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface BatchControlItem {
  id: string;
  ok: boolean;
  status?: string;
  queued?: boolean;
  error?: string;
}

// 一括分類変更のタスク別結果。失敗はタスク削除済みの場合だけで、分類自体の書き込みはアトミック。
export interface BatchCategoryItem {
  id: string;
  ok: boolean;
  error?: string;
}

// Whole-task (all agents) token aggregate.
export interface TokenTotal {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// Global per-profile token spend from the llm_usage ledger (GET /api/tokens/usage).
export interface ProfileUsage {
  profile_name: string;
  calls: number;
  tasks: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// One (profile, UTC day) token bucket for the dashboard's daily chart (new source).
export interface ProfileDayUsage {
  profile_name: string;
  date: string; // YYYY-MM-DD
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

// Response of GET /api/tokens/usage — the dashboard's "new" (llm_usage) token view.
export interface UsageStats {
  by_profile: ProfileUsage[];
  daily: ProfileDayUsage[];
}

// Per-model token usage for one task (GET /api/llm/records/by-model), from the
// always-on llm_usage metering ledger. calls = number of LLM calls on this model.
export interface ModelTokenStat {
  model: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

export interface Session {
  id: string;
  role: SessionRole;
  title: string;
  status: SessionStatus;
  live: boolean;
  last_activity: string;
  intent_id?: string;
  source_task_id?: string;
  inherited?: boolean;
  seg?: number; // main-agent session: which conversation segment (0 = original)
}

// ---- Security ----
export interface AuditEntry {
  ts: string;
  tool: string;
  action: "allow" | "block";
  reason?: string;
  command?: string;
}

export interface Audit {
  entries?: AuditEntry[];
  attributions?: Record<string, number>;
}

// ---- Traffic ----
export interface TrafficExchange {
  id: string;
  ts: string;
  host: string;
  method: string;
  url: string;
  status: number;
  content_type: string;
  resp_len: number;
}

export interface TrafficResp {
  enabled: boolean;
  proxy?: string;
  count?: number; // global total (unfiltered)
  total?: number; // rows matching the current filter (for pagination)
  page?: number;
  size?: number;
  exchanges?: TrafficExchange[];
}

// Full raw request/response of one exchange (lazy-loaded on row select).
export interface TrafficDetail {
  req: string;
  resp: string;
}

// One distinct recorded host with its exchange count (target picker).
export interface TrafficHost {
  host: string;
  count: number;
}

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // Agent によるトラフィック証拠の自動関連付け。デフォルトは無効で、手動関連付けには影響しない
  llm_record: boolean; // LLM 記録スイッチ（デフォルトは無効）；無効時は LLM 呼び出しを一切記録しない
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 独立した出口プロキシ(http/https/socks5)。検索エンドポイントへのアクセス用で、トラフィック記録の MITM プロキシとは無関係。空=直接接続。
  web_search_proxy?: string;
  // グローバル出口プロキシ(http/https/socks5、user:pass も指定可)。全ターゲットのトラフィックに使う。トラフィックキャプチャ有効時は
  // MITM の上流、無効時は agent の bash/WebFetch へ直接注入する。空=直接接続。
  global_proxy?: string;
  python_interpreter?: string; // カスタムスクリプトツールの python インタープリターパス(空=実行時に検出)
  workers?: number; // 並行作業 agent 数(デフォルト3)；以降に開始するタスクへ適用
  // タスク同時実行上限:同時に「実行中」のタスク数上限。無効=無制限;有効後は新規タスクが上限超過なら待機し、空きが出ると自動開始。
  task_concurrency_enabled?: boolean; // デフォルト false
  task_concurrency_limit?: number; // 有効後のデフォルト 5
  // LLM 巡回(フェイルオーバー)。デフォルトは無効；有効にすると「モデル未指定」の agent は現在の設定が使用不能
  // （残高不足/key 無効/レート制限/サービス異常）のとき、次の設定へ自動で切り替わる。
  llm_pool_enabled?: boolean; // デフォルト false
  // 指定設定をバインドした agent/タスクの失敗時も巡回チェーンへフォールバックするか。デフォルト false = バインド先を専用使用。
  llm_pool_bind_fallback?: boolean;
  // 操作制約の注入範囲(デフォルトはすべて有効):タスクの allow/deny 制約を対応する agent のシステムプロンプトへ組み込む。
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
  // 実験機能:noa のモデル駆動コンテキスト圧縮(デフォルトは無効)。有効後はプラットフォームの4種類の agent(planner/
  // worker/メイン agent/会話)のコンテキスト圧縮を noa が担当し、組み込み compaction に代わる;run ごとに1回読み、
  // 以降に開始する run へ適用する。
  noa_compaction?: boolean;
  // ---- 脆弱性 IM 通知（チャネル自体は独立リソース、/api/notify/* を参照。ここには3つのグローバル設定だけ）----
  notify_enabled?: boolean; // 通知全体のスイッチ。デフォルトは有効；保守中の一括停止用
  notify_public_base_url?: string; // 脆弱性詳細への戻りリンクの外部アクセスアドレス；空=メッセージに戻りリンクを含めない
  notify_digest_interval_min?: number; // 集約モードの周期（分）。デフォルト 30
}

// ---- 脆弱性 IM 通知 ----

// NotificationFilter はチャネルのフィルター条件。全フィールド省略可能で、省略するとフィルターしない。
// バックエンドはどのフィールドにも検証を加えない：設定が不正なら「一致」とする（通知漏れより過剰通知を選ぶ）。
export interface NotificationFilter {
  min_severity?: string; // "" | low | medium | high | critical
  task_ids?: number[]; // 空=無制限；空でなければ脆弱性の所属タスクとの共通部分が必要
  asset_ids?: number[]; // 空=無制限；空でなければ脆弱性に紐付く資産との共通部分が必要
  vulnclass_include?: string[]; // 空=すべて受信；空でなければ脆弱性の種類がいずれかのキーワードに一致する必要がある（大文字・小文字を区別しない部分文字列）
  vulnclass_exclude?: string[]; // いずれかのキーワードに一致すれば除外（除外が包含より優先）
  on_status_change?: boolean; // 脆弱性の対処状態変更イベントも受信するか
}

// NotificationChannel はチャネルのインスタンス。config のフィールドは kind によって異なり、
// 認証フィールドは読み取り時に "__masked__" で始まるマスク値へ置換する——そのまま返すと「変更しない」を表す。
export interface NotificationChannel {
  id: number;
  name: string;
  kind: string;
  enabled: boolean;
  mode: "realtime" | "digest";
  config: Record<string, unknown>;
  filter: NotificationFilter;
  rate_per_min: number;
  created_at: string;
  updated_at: string;
  // secret_keys はバックエンドがチャネルの種類に応じて示し、フロントエンドはそれを基にパスワード欄と「空欄なら変更しない」の案内を描画し、
  // チャネル固有の知識をハードコードしない。
  secret_keys: string[];
}

// NotificationKind は /api/notify/meta が返すチャネルの種類のメタデータ。
export interface NotificationKind {
  kind: string;
  default_rate_per_min: number;
  secret_keys: string[];
}

export interface NotificationMeta {
  kinds: NotificationKind[];
  enabled: boolean;
  public_base_url: string;
  digest_interval_min: string;
  defaults: { digest_interval_min: number };
  stats: {
    channels: number;
    channels_on: number;
    pending: number;
    failed: number;
    sent_today: number;
    backlog_age_ms: number;
  };
}

// NotificationDelivery は配信記録1件で、配信履歴と失敗時の再送に使う。
export interface NotificationDelivery {
  id: number;
  finding_id: string;
  event_kind: string; // finding_created | finding_status_changed
  channel_id: number;
  channel_name: string;
  channel_kind: string;
  state: "pending" | "sending" | "sent" | "failed" | "skipped";
  attempts: number;
  last_error: string;
  batch_id?: number;
  created_at: string;
  sent_at?: string;
  next_attempt_at: string;
  title: string;
  severity: string;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 思考スイッチ(thinking.type): ""=送信しない(デフォルト) | "disabled"=無効 | "enabled"=有効
  thinking_type?: string;
  // 思考強度: ""=送信しない(デフォルト) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // 巡回順位：大きいほど先に選択する。有効な設定はこの値に関係なく常にチェーンの先頭。
  priority?: number;
  // true = フェイルオーバー先にしない（agent/タスクによる明示的なバインド使用は可能）。
  pool_exclude?: boolean;
  // true（デフォルト）= ストリーミング(SSE) | false = 実際の非ストリーミング(stream:false、一括返却)。
  streaming?: boolean;
  // 1回の応答の出力上限(token)。0 = このフィールドを送信せず、サーバーの既定値で決める。
  // context_window_k と区別すること：後者はモデルの総容量で、ローカルの圧縮閾値だけに使う。
  max_tokens?: number;
  // 上限に使うリクエストフィールド名。format="openai" のみ意味を持つ：
  // ""=max_tokens(デフォルト) | "max_completion_tokens"(OpenAI 推論モデルはこれだけを認識)
  max_tokens_field?: string;
  // カスタムセッションヘッダー名：空でない場合は各リクエストへこの HTTP ヘッダーを付け、値=現在のセッション/インテントの session id。
  // ""=送信しない。session-id ヘッダーでプロンプトキャッシュ/スティッキールーティングを行うゲートウェイ用。
  session_header_key?: string;
  // この設定の再試行上書き（接続/空応答/同じ provider の安全ウィンドウ）。空欄/すべて 0 = グローバル戦略に従う。
  retry?: LLMRetryOverride;
}

// ---- LLM 再試行戦略 ----
// 1層の再試行の2つの調整項目。どちらも「0 = 未設定」：
//   attempts    0=既定回数を使う | -1=この層の再試行を無効化 | >0=再試行回数
//   interval_ms 0=既定の指数バックオフを使う | >0=この固定ミリ秒間隔へ変更
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// 個々の LLM 設定で上書きできる3層（いずれも「エンドポイントに付随する」再試行）。
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 接続再試行：接続リセット/タイムアウト/429/5xx、ストリーム開始前
  empty: LLMRetryRule; // 空応答の再試行：完了しても内容がない（openai 形式のみ）
  stream: LLMRetryRule; // 同じ provider の安全ウィンドウでの再試行：出力提供前のストリーム切断を再実行
}

// グローバル戦略 = 上記3層の既定値 + グローバルだけの2層：
//   breaker 巡回のサーキットブレーク（attempts=連続一時失敗何回で遮断するか、interval_ms=固定クールダウン時間）
//   intent  インテント再実行（worker が model_error で終了した後にインテント全体を再実行）
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM 巡回（フェイルオーバー）----
// 巡回チェーン内の設定の位置と正常性。state:
//   ok       正常
//   degraded 連続失敗があるがサーキットブレーク閾値未満
//   tripped  遮断済みで、クールダウン中はスキップ（cooldown_secs は残り秒数）
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 現在有効な設定か（常にチェーン先頭）
  excluded: boolean; // pool_exclude：巡回に参加しない
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 組み込みは goals/planner/mainagent/worker；カスタムはユーザー定義 key
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // バインドした LLM 設定；null/absent = タスク/セッション/グローバル設定に従う
  max_turns?: number; // 0 = 無制限
  run_seconds?: number; // worker の1回の実行の実時間上限(秒)；0 = 無制限
  web_search?: boolean; // ネット検索を有効にするか(システム全体のスイッチで制御)
  interactive_shell?: boolean; // 対話型 shell を有効にするか(永続 PTY セッションツール群)
  // P3 トリガー後の処理方針(カスタム agent のみ意味がある)
  trigger_run_mode?: "serial" | "parallel"; // 直列キュー / 発火ごとに個別セッションを並行実行
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial のみ：同じタスクを統合 / すべて統合 / 統合しない
  trigger_max_parallel?: number; // parallel のみ：agent ごとの同時実行上限；0=無制限
  // バインド数(一覧インターフェースのみ返す)：可視 MCP / 可視 Skill / バインドしたツール
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // バインド可能な LLM 設定候補(「デフォルトモデル」ドロップダウン用)；現在のバインドは agent.llm_profile_id を参照
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 保存済みの終了プロンプト(空=組み込み既定値)
  wrapup_default?: string; // 組み込み既定の終了プロンプト(プレースホルダー/既定値へ復元)
  wrapup_max_turns?: number; // 保存済みの終了ラウンド数(0=組み込み既定値)
  wrapup_max_turns_default?: number; // 組み込み既定の終了ラウンド数("0=デフォルトN" 表示用)
  // タスク単位のタイムアウト終了プロンプト(worker/planner のみ、task_timeout_wrapup_supported=true の場合だけこの区画を表示)
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}

// ---- MCP ----
export interface MCPServer {
  id: number;
  name: string;
  transport: "stdio" | "http" | "sse";
  command?: string;
  args: string[];
  env: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure?: boolean; // http: skip TLS cert verification (self-signed servers)
  tools?: string[]; // mcp_tools_cache (names only, for the count)
}

export interface MCPTool {
  name: string;
  description: string;
}

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 呼び出し統計（skill_usage 台帳）。未使用の skill：calls=0、last_used は省略。
  calls: number;
  tasks: number; // 読み込んだタスク数（chat セッションは含めない）
  usage_agents: string[]; // 読み込んだ agent key
  last_used?: string;
}

// SkillCall は1回の Skill() 呼び出し（単一 skill の最近の呼び出し一覧）。
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 = タスク以外の場面（会話セッション）
  session_id: string;
  args_len: number;
}

// MissingSkill は指名されたが存在しない skill —— "使いたいがない"という不足。
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}

// ---- Tools (組み込みツール一覧) ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // カスタムツールの種類
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  exec?: Record<string, any>; // カスタムツールの実行仕様(kind!=builtin)
  deferred?: boolean; // schema の遅延(SearchExtraTools/ExecuteExtraTool)
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}

// ---- Intercept Rules ----
export type InterceptAction = "allow" | "deny" | "ask";
export type InterceptMatchTarget = "tool_name" | "tool_input";
export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

// ---- Asset Intercept Rules（資産インターセプト：グローバルブラックリスト） ----
export type AssetInterceptKind =
  | "exact_domain"
  | "exact_ip"
  | "exact_url"
  | "fuzzy_domain"
  | "fuzzy_ip"
  | "fuzzy_url"
  | "cidr";

// action はタスク単位ルール専用：block=ブロック(テスト禁止) allow=許可(ホワイトリスト)。
export type AssetInterceptAction = "block" | "allow";

// タスク単位の資産インターセプト/許可ルールの入力項目（タスク作成、タスク詳細編集で使用）。
export interface AssetInterceptRuleInput {
  action: AssetInterceptAction;
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  enabled: boolean;
}

export interface AssetInterceptRule {
  id: number;
  enabled: boolean;
  action?: AssetInterceptAction; // グローバルルールにはこのフィールドがない（常にブロック）；タスク単位ルールは block/allow を区別する
  kind: AssetInterceptKind;
  pattern: string;
  note: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // ルールの message またはモデルの判定理由(モデル判定は [模型] 接頭辞付き)
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: モデルによる補完承認(インターセプトルールが1つも一致しない場合のみモデルが判断)のグローバル設定。
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 = 有効/デフォルト設定に従う
  prompt: string; // 判定プロンプト;GET で未設定ならバックエンドが組み込みテンプレート全文を補充
  timeout_seconds: number; // モデル呼び出しのタイムアウト
  fail_action: "allow" | "ask" | "deny"; // モデルのエラー/タイムアウト/解析不能時のフォールバック
  ask_timeout_seconds: number; // モデルが ask と判定し人手へ回した後の承認待ちタイムアウト
  ask_timeout_action: "allow" | "deny"; // 承認タイムアウト後のデフォルト動作
}

// JudgeUsage: モデルによる補完承認(judge チャネル)の累積 token 使用量 + 直近 N 日の日別系列。
export interface JudgeDayUsage {
  date: string; // YYYY-MM-DD (UTC)
  calls: number;
  input_tokens: number;
  output_tokens: number;
}
export interface JudgeUsage {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  daily: JudgeDayUsage[];
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "unknown";
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// ── 資産同期 (ScopeSentry データソース) ──────────────────────────────────────────────
export interface SSProject {
  id: string; // MongoDB ObjectID — used as filter.project
  name: string;
  logo?: string;
  AssetCount?: number;
  tag?: string;
}

export interface SSTask {
  id: string;
  name: string; // used as filter.task
  status?: number;
  progress?: number;
  creatTime?: string;
  endTime?: string;
}

// ConvTokenSummary — one conversation's token total (+ profile/date) for merging
// chat usage into the dashboard token stats. GET /api/tokens/conversations.
export interface ConvTokenSummary {
  llm_profile_id: number | null;
  created_at: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
}

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// 単一ツールの呼び出し統計（/commands/stats）；errors はそのうちの失敗回数。
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider が実際に送受信した HTTP 原文：リクエストは buildBody() が送信した完全な body（ツールの
  // schema を含む）、レスポンスは元の SSE フレーム。上の request_body/response_body は正規化ビューで、
  // ツールの schema と tool_use ブロックを除いている。古い記録は空。
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}
export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

export type TrafficEvidenceRole = "baseline" | "proof" | "verification" | "supporting";
export interface TrafficEvidenceRef {
  traffic_id: string;
  role?: TrafficEvidenceRole;
  note?: string;
}
export interface TrafficEvidenceSnapshot {
  id: string;
  source_traffic_id: string;
  captured_at: number;
  url: string;
  method: string;
  status: number;
  content_type: string;
  req_head?: string;
  resp_head?: string;
  req_hash: string;
  resp_hash: string;
  req_len: number;
  resp_len: number;
}
export interface FindingTrafficBinding {
  id: string;
  finding_id: string;
  snapshot_id: string;
  role: TrafficEvidenceRole;
  note: string;
  position: number;
  created_at: string;
  snapshot: TrafficEvidenceSnapshot;
}
export interface FindingTraffic {
  finding_id: string;
  version: number;
  report_version: number;
  bindings: FindingTrafficBinding[];
}
export interface EvidenceBodyPreview {
  content: string;
  offset: number;
  total: number;
  next_offset: number;
  truncated: boolean;
  binary: boolean;
}
export interface FindingTrafficDetail {
  binding: FindingTrafficBinding;
  request: EvidenceBodyPreview;
  response: EvidenceBodyPreview;
}

/** GET /api/update/check —— 現在のバージョンと GitHub の最新正式版の比較結果。 */
export interface UpdateCheck {
  /** 現在実行中のバージョン；開発ビルドは "dev" または git describe の接尾辞付き形式。 */
  current: string;
  /** 実行形態。docker での入れ替えはコンテナーの書き込み層のみに作用し、コンテナー再作成でイメージの版に戻る。 */
  mode: "docker" | "binary";
  os: string;
  arch: string;
  repo: string;
  /** ロールバック可能な前のバージョン（artex.old）があるか。 */
  has_backup: boolean;
  /** 今回の起動時の自己更新ブートストラップ結果（入れ替え失敗 / ロールバック済みなど）。何もなければ空。 */
  boot_notice?: string;
  rolled_back?: boolean;
  /** GitHub 検索失敗時の理由。この場合、以下のフィールドはすべて存在しない。 */
  error?: string;
  latest?: string;
  notes?: string;
  html_url?: string;
  published_at?: string;
  /** 現在のプラットフォームに対応する配布パッケージ名と、その Release が実際にそれを含むか。 */
  asset?: string;
  asset_available?: boolean;
  size?: number;
  has_update?: boolean;
  /** 両方のバージョン番号を比較できるか；開発ビルドは false で、ワンクリック更新を無効にする。 */
  comparable?: boolean;
  /** comparable が false の場合の説明。 */
  reason?: string;
}

/** /api/update/stream が配信する更新進捗1件。 */
export interface UpdateProgress {
  phase: "idle" | "downloading" | "verifying" | "extracting" | "staged" | "failed";
  /** ダウンロード段階のみ意味を持つ（0-100）；その他の段階は -1。 */
  percent: number;
  message: string;
  version?: string;
  error?: string;
}

// Original execution selected from an approval, never submitted to the reviewer.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}
