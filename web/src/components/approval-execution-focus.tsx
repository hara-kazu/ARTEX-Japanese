"use client";

import * as React from "react";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { Activity, InterceptExecution } from "@/lib/types";

// Resolve one exact persisted call, independently of transcript pagination.
export function useApprovalFocus({ taskId, conversationId }: { taskId?: string; conversationId?: number }) {
  const [state, setState] = React.useState<{
    id: number;
    source?: InterceptExecution;
    error?: string;
    loading: boolean;
  } | null>(null);
  const [revision, setRevision] = React.useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: revision is the explicit retry trigger.
  React.useEffect(() => {
    const q = new URLSearchParams(window.location.search);
    const id = Number(q.get("approval"));
    const matches = conversationId ? q.get("c") === String(conversationId) : q.get("id") === taskId;
    if (!matches || !Number.isSafeInteger(id) || id <= 0) {
      setState(null);
      return;
    }
    let cancelled = false;
    setState({ id, loading: true });
    api
      .interceptExecution(id, conversationId)
      .then((source) => {
        if (cancelled) return;
        if (
          conversationId
            ? source.conversation_id !== conversationId
            : source.task_id !== taskId || source.conversation_id != null
        ) {
          throw new Error("承認の発生元が現在のセッションと一致しません");
        }
        setState({ id, source, loading: false });
      })
      .catch((e) => {
        if (!cancelled) setState({ id, error: (e as Error).message || "元の実行を特定できません", loading: false });
      });
    return () => {
      cancelled = true;
    };
  }, [taskId, conversationId, revision]);
  const close = React.useCallback(() => {
    const url = new URL(window.location.href);
    url.searchParams.delete("approval");
    window.history.replaceState(null, "", url);
    setState(null);
  }, []);
  return { state, close, retry: () => setRevision((v) => v + 1) };
}

type HistoryPage = { items: Activity[]; hasMore: boolean };

// Fill the existing transcript continuously back to the call, preserving all
// intervening messages. Never splice an isolated call into a paginated history.
export function useApprovalHistory(
  source: InterceptExecution | undefined,
  loaded: boolean,
  items: Activity[],
  loadPage: (before: number) => Promise<HistoryPage>,
  mergePage: (page: HistoryPage) => void,
) {
  const itemsRef = React.useRef(items);
  itemsRef.current = items;
  const [result, setResult] = React.useState<{ source: InterceptExecution; error?: string }>();
  React.useEffect(() => {
    if (!source || !loaded) return;
    let cancelled = false;
    void (async () => {
      let current = itemsRef.current;
      let before = current[0]?.seq ?? 0;
      while (!current.some((a) => a.seq === source.seq && a.kind === "tool_use")) {
        const page = await loadPage(before);
        if (cancelled) return;
        if (!page.items.length || (before > 0 && page.items[0].seq >= before)) {
          throw new Error("対応するツール呼び出しがセッションに見つかりません。削除された可能性があります");
        }
        mergePage(page);
        current = page.items;
        before = current[0].seq;
        if (!page.hasMore && !current.some((a) => a.seq === source.seq)) {
          throw new Error("対応するツール呼び出しがセッションに見つかりません");
        }
      }
      if (!cancelled) setResult({ source });
    })().catch((error) => {
      if (!cancelled) setResult({ source, error: (error as Error).message });
    });
    return () => {
      cancelled = true;
    };
  }, [source, loaded, loadPage, mergePage]);
  return {
    ready: !!source && result?.source === source && !result.error,
    error: result?.source === source ? result?.error : undefined,
  };
}

export function ApprovalExecutionFocus({
  focus,
  history,
}: {
  focus: ReturnType<typeof useApprovalFocus>;
  history: ReturnType<typeof useApprovalHistory>;
}) {
  const { state } = focus;
  if (!state) return null;
  const error = state.error || history.error;
  return (
    <div
      role="status"
      className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b px-4 py-2 text-xs"
    >
      <span className={error ? "text-destructive" : "text-muted-foreground"}>
        {error
          ? `特定できません：${error}`
          : history.ready
            ? `承認を展開しました #${state.id} に対応するツール呼び出し`
            : `承認の位置を読み込んでいます #${state.id} のチャット内の位置…`}
      </span>
      <div className="flex gap-2">
        {error ? (
          <Button size="sm" variant="outline" onClick={focus.retry}>
            位置の特定を再試行
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" onClick={focus.close}>
          位置の特定をキャンセル
        </Button>
      </div>
    </div>
  );
}
