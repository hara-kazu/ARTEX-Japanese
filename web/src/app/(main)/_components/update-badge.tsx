"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * 上部バーの"新バージョンあり"表示：ページ全体の読み込み時に1回確認し、更新があればバージョン番号の横に表示する。
 * クリックするとシステム設定ページの「バージョンと更新」カードへ直接移動する。
 *
 * バックエンドは GitHub の検索結果を 30 分キャッシュするため、ここでマウントごとに1回確認しても問題ない
 * ——未認証の GitHub API は 60 回/時/IP のみで、そのキャッシュがないと複数タブを開くだけで
 * 割り当てを使い切り、実際に更新したいときに確認できなくなる。
 *
 * 検索失敗は常に何も表示しない：上部バーはエラー表示の場所ではなく、設定ページで「更新を確認」を押せば理由が見える。
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update は既に"バージョン番号が比較可能"の判定を含み、開発ビルドではこの表示を出さない。
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 表示しない：ネット未接続 / GitHub のレート制限で上部バーにエラーを出してはならない。
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`新しいバージョンが見つかりました： ${latest}。クリックして更新画面へ`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 脈動する点：上部バーには要素が多く、文字だけでは見落としやすいため、動きで一目で気付けるようにする。 */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">新しいバージョン {latest}</span>
      <span className="sm:hidden">新しいバージョン</span>
    </Link>
  );
}
