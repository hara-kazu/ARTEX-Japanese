"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 新バージョンの起動を待つ最大時間。1回の更新に3回のプロセス起動（一時保存 → 入れ替え → 新版）が必要で、
 *  それぞれ秒単位のため、3分あれば遅いディスクと Docker コンテナー再作成もカバーできる。 */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // progress とは分離する：一時保存完了後にプロセスが終了して SSE が切れるため、その時点で /api/health のポーリングへ切り替える。
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet はバックエンドキャッシュを迂回するかも決める：ページを開いた際の自動確認はキャッシュを使い（上部バーで確認済み）、
  // ユーザーが「更新を確認」を手動クリックした場合は必ず配信元へ問い合わせ、公開直後の版がキャッシュ期限切れまで見えないのを避ける。
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("更新の確認に失敗しました：" + r.error);
          else if (r.has_update) toast.success(`新しいバージョンが見つかりました： ${r.latest}`);
          else if (r.comparable) toast.success("最新バージョンです");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("更新の確認に失敗しました：" + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // バージョン番号が変わるまで /api/health をポーリングする。
  //
  // 判定基準は"接続できた"ではなく"バージョンが変わった"でなければならない：入れ替え中に旧版が一時的に1回再起動し、
  // （その回は artex.new への入れ替えだけを行ってすぐ終了する）接続性だけでは成功と誤判定する。
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`更新しました： ${j.version}。ページを再読み込みしています`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 再起動中に接続できないのは想定内なので、ポーリングを続ける。
      }
    }
    setRestarting(false);
    toast.error("サービスの再起動を待機中にタイムアウトしました。バックエンドのログ、または artex を start.sh / start.bat で起動したかを確認してください。");
  }, []);

  // 更新進捗を購読する。SSE は Next の /api 書き換えを通さない（その層がバッファリングし、イベントを送出できないため）。
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("更新に失敗しました：" + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // プロセス終了時に SSE は必ず切れる。既に再起動待ちに入っていれば正常な動作なので、
        // /api/health のポーリングで判定を続ければよい。
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `更新しますか： ${info.latest}？\n\n` +
        "更新するとプログラムを再起動し、実行中のタスクが中断されます。\n" +
        (info.mode === "docker"
          ? "\n注意：コンテナー内の更新はプログラム本体のみで、イメージ内の playwright / nmap などのツールは更新しません。" +
            "新しいバージョンが新しいツールを必要とする場合は、docker compose pull を使用してください。"
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "準備中…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("更新の開始に失敗しました：" + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "前のバージョンに戻しますか？\n\nプログラムを再起動し、実行中のタスクが中断されます。\n注意：データベース構造は戻りません。旧バージョンでは新しいデータを認識できない場合があります。",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("前のバージョンに切り替えました。再起動しています…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("ロールバックに失敗しました：" + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 実際のパーセントを取得できるのはダウンロード段階のみ（Content-Length で計算）。検証/展開/再起動待ちはどれも
  // 所要時間が分からない段階なので、進捗バーを満たして脈動アニメーションを付け、"処理中だが残り時間は不明"を表す。
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 設定ページは複数列のウォーターフォールレイアウトで、カード自身が行間隔を担当して列をまたぐ分割を禁止する（page.tsx のコメントを参照）。
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          バージョンと更新
        </CardTitle>
        <CardDescription>GitHub で新しいバージョンを確認し、インストールします。更新時に再起動し、実行中のタスクが中断されます。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">現在のバージョン</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "スタンドアロン"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">最新バージョン</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              変更履歴 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            GitHub に接続できません：{info.error}
            {"　"}上で全体のプロキシを設定してから再試行してください。
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} には {info.os}/{info.arch} 向けのリリースパッケージがありません（不足：{info.asset}）。自動更新はできません。
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            ダウンロード： <span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}。SHA256 の検証とスモークテスト後に置き換えます。失敗した場合は現在のバージョンを保持します。
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            最新バージョンです。
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker 内の更新はプログラム本体のみを置き換え、イメージ内の playwright / nmap などのツールは更新しません。また、
            <span className="font-mono"> docker compose up -d </span>
            でコンテナーを再作成すると、イメージに含まれるバージョンに戻ります。イメージも更新する場合は、次を実行してください：
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "再起動して新しいバージョンを適用しています。お待ちください（ページは自動更新されます）…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            更新を確認
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `更新先： ${info.latest}` : "今すぐ更新"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              前のバージョンに戻す
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          ワンクリック更新は監視スクリプトによる再起動が必要です。次のスクリプトで <span className="font-mono">start.sh</span>（Windows は
          <span className="font-mono"> start.bat</span>）ARTEX を起動してください。artex 本体を直接実行した場合、終了後に自動再起動しません。
        </p>
      </CardContent>
    </Card>
  );
}
