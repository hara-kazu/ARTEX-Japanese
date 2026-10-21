"use client";

import Link from "next/link";

import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <div className="flex h-dvh flex-col items-center justify-center space-y-2 text-center">
      <h1 className="font-semibold text-2xl">ページが見つかりません。</h1>
      <p className="text-muted-foreground">お探しのページが見つかりませんでした。</p>
      <Link prefetch={false} replace href="/function/tasks">
        <Button variant="outline">ホームに戻る</Button>
      </Link>
    </div>
  );
}
