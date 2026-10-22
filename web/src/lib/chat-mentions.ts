export const mentionKinds = [
  { kind: "finding", label: "脆弱性", alias: "finding", legacyLabel: "漏洞" },
  { kind: "asset", label: "資産", alias: "asset", legacyLabel: "资产" },
  { kind: "company", label: "企業", alias: "company", legacyLabel: "企业" },
  { kind: "endpoint", label: "API", alias: "api", legacyLabel: "接口" },
  { kind: "ip", label: "IP", alias: "ip", legacyLabel: "IP" },
  { kind: "app", label: "アプリ", alias: "app", legacyLabel: "应用" },
  { kind: "root_domain", label: "ドメイン", alias: "domain", legacyLabel: "域名" },
  { kind: "subdomain", label: "サブドメイン", alias: "subdomain", legacyLabel: "子域名" },
  { kind: "service", label: "サービス", alias: "service", legacyLabel: "服务" },
] as const;

export type MentionKind = (typeof mentionKinds)[number]["kind"];
export interface ChatMention {
  kind: MentionKind;
  id: number;
  label: string;
  description: string;
}

export function activeMention(value: string, caret: number) {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf("@");
  if (start < 0 || (start > 0 && /[\w.+/-]/.test(before[start - 1]))) return null;
  const query = before.slice(start + 1);
  if (/[[\]\r\n@]/.test(query) || query.length > 220) return null;
  return { start, end: caret, query };
}

export function mentionSearch(query: string) {
  const text = query.trimStart().toLowerCase();
  for (const item of mentionKinds) {
    for (const alias of [item.label.toLowerCase(), item.legacyLabel.toLowerCase(), item.alias]) {
      if (text === alias || text.startsWith(`${alias} `) || (/[^a-z]/.test(alias) && text.startsWith(alias))) {
        return { kind: item.kind, query: query.trimStart().slice(alias.length).trim(), categories: [] };
      }
    }
  }
  const categories = mentionKinds.filter(
    (item) => item.label.toLowerCase().startsWith(text) || item.legacyLabel.toLowerCase().startsWith(text) || item.alias.startsWith(text),
  );
  return { kind: "" as const, query: query.trim(), categories };
}

export function mentionToken(item: ChatMention) {
  const kind = mentionKinds.find((entry) => entry.kind === item.kind)?.label ?? "資産";
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "（" : "）"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${kind}#${item.id} ${label}]`;
}

export function selectedMentions(value: string) {
  return [...value.matchAll(/@\[(脆弱性|資産|企業|API|IP|アプリ|ドメイン|サブドメイン|サービス|漏洞|资产|企业|接口|应用|域名|子域名|服务)#([0-9]+)(?: ([^\]\r\n]*))?\]/g)].map(
    (match) => ({
      token: match[0],
      label: `${mentionKinds.find((kind) => kind.label === match[1] || kind.legacyLabel === match[1])?.label ?? match[1]} #${match[2]}${match[3] ? ` · ${match[3]}` : ""}`,
      start: match.index,
    }),
  );
}
