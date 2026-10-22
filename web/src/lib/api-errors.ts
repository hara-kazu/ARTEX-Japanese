import errorMessages from "./api-error-messages.json" with { type: "json" };

type MessageOptions = {
  diagnosticArguments?: number[];
  suffix?: "diagnostic" | "value";
};

const messages = errorMessages as [string, string, MessageOptions?][];

const placeholder = /%[sdqwv]/g;
const escapeRegex = (value: string) => value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const exactMessages = new Map(
  messages.filter(([source]) => !source.includes("%")).map(([source, target]) => [source, target] as const),
);
const templates = messages
  .filter(([source]) => source.includes("%"))
  .map(([source, target, options]) => ({
    pattern: new RegExp(`^${source.split(placeholder).map(escapeRegex).join("([\\s\\S]+?)")}$`),
    greedyPattern: new RegExp(`^${source.split(placeholder).map(escapeRegex).join("([\\s\\S]+)")}$`),
    target,
    diagnosticArguments: new Set(
      [...source.matchAll(placeholder)]
        .map(([format], index) => (format === "%w" || options?.diagnosticArguments?.includes(index) ? index : -1))
        .filter((index) => index >= 0),
    ),
  }));
const prefixes = messages
  .filter(([, , options]) => options?.suffix)
  .toSorted(([left], [right]) => right.length - left.length);

export function localizeApiError(message: string): string {
  return translate(message, 0) ?? message;
}

export function localizeApiErrorBody(body: string): string {
  try {
    const parsed = JSON.parse(body);
    const localized = localizeApiResponse(parsed);
    if (localized !== parsed) return JSON.stringify(localized);
  } catch {}
  return localizeApiError(body);
}

export function localizeApiResponse<T>(value: T): T {
  if (!value || typeof value !== "object") return value;
  if (Array.isArray(value)) {
    const localized = value.map(localizeApiResponse);
    return (localized.some((entry, index) => entry !== value[index]) ? localized : value) as T;
  }
  const original = value as Record<string, unknown>;
  let result = original;
  const update = (key: string, next: unknown) => {
    if (next === original[key]) return;
    if (result === original) result = { ...original };
    result[key] = next;
  };
  for (const key of ["error", "last_error"]) {
    if (typeof original[key] === "string") update(key, localizeApiError(original[key]));
  }
  for (const key of ["errors", "warnings", "scope_errors"]) {
    const entries = original[key];
    if (!Array.isArray(entries)) continue;
    const localized = entries.map((entry) => (typeof entry === "string" ? localizeApiError(entry) : entry));
    if (localized.some((entry, index) => entry !== entries[index])) update(key, localized);
  }
  for (const key of ["items", "deliveries"]) {
    const entries = original[key];
    if (!Array.isArray(entries)) continue;
    const localized = entries.map(localizeApiResponse);
    if (localized.some((entry, index) => entry !== entries[index])) update(key, localized);
  }
  return result as T;
}

function translate(message: string, depth: number): string | null {
  if (depth > 4) return message;
  const exact = exactMessages.get(message);
  if (exact) return exact;
  for (const { pattern, greedyPattern, target, diagnosticArguments } of templates) {
    const match = pattern.exec(message);
    if (!match) continue;
    const greedyMatch = greedyPattern.exec(message);
    if (match.some((value, index) => value !== greedyMatch?.[index])) return null;
    let index = 0;
    let ambiguous = false;
    const localized = target.replace(placeholder, () => {
      const value = match[++index];
      if (!diagnosticArguments.has(index - 1)) return value;
      const translated = translate(value, depth + 1);
      if (translated === null) ambiguous = true;
      return translated ?? value;
    });
    return ambiguous ? null : localized;
  }
  for (const [source, target, options] of prefixes) {
    if (!message.startsWith(source)) continue;
    const suffix = message.slice(source.length);
    const localized = options?.suffix === "diagnostic" ? translate(suffix, depth + 1) : suffix;
    return localized === null ? null : target + localized;
  }
  return message;
}
