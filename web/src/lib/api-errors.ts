import errorMessages from "./api-error-messages.json" with { type: "json" };

// Display translations for the existing wire messages; identifiers stay unchanged.
const messages = errorMessages.map(([source, target]) => [source, target] as const);

const placeholder = /%[sdqwv]/g;
const escapeRegex = (value: string) => value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const exactMessages = new Map(messages.filter(([source]) => !source.includes("%")));
const templates = messages
  .filter(([source]) => source.includes("%"))
  .map(([source, target]) => ({
    pattern: new RegExp(`^${source.split(placeholder).map(escapeRegex).join("([\\s\\S]+?)")}$`),
    target,
  }));
const prefixes = messages
  .filter(([source]) => !source.includes("%") && /[：:(（]\s*$/.test(source))
  .toSorted(([left], [right]) => right.length - left.length);

/** Translate known backend errors while retaining IDs, paths and unknown diagnostics. */
export function localizeApiError(message: string): string {
  return translate(message, 0);
}

function translate(message: string, depth: number): string {
  if (depth > 4) return message;
  const exact = exactMessages.get(message);
  if (exact) return exact;
  for (const { pattern, target } of templates) {
    const match = pattern.exec(message);
    if (!match) continue;
    let index = 0;
    return target.replace(placeholder, () => translate(match[++index], depth + 1));
  }
  for (const [source, target] of prefixes) {
    if (message.startsWith(source)) return target + translate(message.slice(source.length), depth + 1);
  }
  return message;
}
