"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// セッション入力欄の送信/改行キー。フロントエンドだけの設定：localStorage のみに保存し、データベースやアカウント同期には含めないため、
// ブラウザー変更時は再設定が必要。issue #39 を参照——0.3.2 で Ctrl+Enter 送信から Enter 送信に変更したため、
// ここで以前のキーを選択肢として戻す。
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter で送信、Shift+Enter で改行" },
  { value: "ctrl-enter", label: "Ctrl+Enter で送信、Enter で改行" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// 同じタブ内の購読者集合。localStorage の storage イベントは「他の」タブだけで発火するため、
// このページの設定変更後は emit で同じタブの入力欄に通知する必要があり、ないと再読み込みまで反映しない。
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// 返すのは文字列リテラルで、Object.is は値で比較するため、useSyncExternalStore がループしない。
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// サーバーには localStorage がなく、先に既定値を描画し、hydrate 後に getSnapshot が修正する。
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey は1回のキー入力で送信すべきか判定する。
// isComposing / keyCode 229 は中国語などの入力方式で文字選択中のため、通す必要があり、そうしないとエンターでの候補選択が誤送信になる。
// enter モードは Shift だけを除外し、0.3.2 の動作と完全に一致させる——設定を変えないユーザーの操作感は変わらない。
// ctrl-enter モードは Ctrl と Cmd（macOS）の両方を受け付ける。
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
