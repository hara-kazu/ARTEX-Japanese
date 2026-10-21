/**
 * Script: generate-theme-presets.ts
 *
 * This script scans the /styles/presets directory for CSS files containing theme definitions.
 * It extracts `label:`, `value:`, and primary color definitions (`--primary`) for both light and dark modes.
 * These primary colors are used to visually represent each theme in the UI (e.g., colored dots or theme previews).
 * Default theme colors are fetched from /app/globals.css.
 * All extracted metadata is injected into a marked section of the /lib/preferences/theme.ts file.
 *
 * Usage:
 * - During local development, run manually after adding any new theme preset:
 *     npm run generate:presets
 * - Ensure that each new CSS preset includes `label:` and `value:` comments.
 * - This generation step is currently automated using a Husky pre-push hook.
 * - You may optionally integrate it directly into a build step if preferred.
 */

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

const presetDir = path.resolve(__dirname, "../styles/presets");

if (!fs.existsSync(presetDir)) {
  console.error(`❌ プリセットのディレクトリが見つかりません：${presetDir}`);
  process.exit(1);
}

const outputPath = path.resolve(__dirname, "../lib/preferences/theme.ts");

const files = fs.readdirSync(presetDir).filter((file) => file.endsWith(".css"));

if (files.length === 0) {
  console.warn("⚠️ プリセットの CSS がありません。デフォルトのプリセットのみを使用します。");
}

const presets = files.map((file) => {
  const filePath = path.join(presetDir, file);
  const content = fs.readFileSync(filePath, "utf8");

  const labelMatch = content.match(/label:\s*(.+)/);
  const valueMatch = content.match(/value:\s*(.+)/);

  if (!labelMatch) {
    console.warn(`⚠️ ${file} に label: がありません。ファイル名を使用します。`);
  }
  if (!valueMatch) {
    console.warn(`⚠️ ${file} に value: がありません。ファイル名を使用します。`);
  }

  const presetLabels: Record<string, string> = {
    Brutalist: "ブルータリスト",
    "Soft Pop": "ソフトポップ",
    Tangerine: "タンジェリン",
  };
  const originalLabel = labelMatch?.[1]?.trim() ?? file.replace(".css", "");
  const label = presetLabels[originalLabel] ?? originalLabel;
  const value = valueMatch?.[1]?.trim() ?? file.replace(".css", "");

  const lightPrimaryMatch = content.match(/:root\[data-theme-preset="[^"]*"\][\s\S]*?--primary:\s*([^;]+);/);
  const darkPrimaryMatch = content.match(/\.dark:root\[data-theme-preset="[^"]*"\][\s\S]*?--primary:\s*([^;]+);/);

  const primary = {
    light: lightPrimaryMatch?.[1]?.trim() ?? "",
    dark: darkPrimaryMatch?.[1]?.trim() ?? "",
  };

  if (!lightPrimaryMatch || !darkPrimaryMatch) {
    console.warn(`⚠️ ${file} のライトまたはダーク用の --primary がありません。CSS の構文を確認してください。`);
  }

  return { label, value, primary };
});

const globalStylesPath = path.resolve(__dirname, "../app/globals.css");

let globalContent = "";
try {
  globalContent = fs.readFileSync(globalStylesPath, "utf8");
} catch (err) {
  console.error(`❌ globals.css を読み取れません：${globalStylesPath}`);
  console.error(err);
  process.exit(1);
}

const defaultLightPrimaryRegex = /:root\s*{[^}]*--primary:\s*([^;]+);/;
const defaultDarkPrimaryRegex = /\.dark\s*{[^}]*--primary:\s*([^;]+);/;

const defaultLightPrimaryMatch = defaultLightPrimaryRegex.exec(globalContent);
const defaultDarkPrimaryMatch = defaultDarkPrimaryRegex.exec(globalContent);

const defaultPrimary = {
  light: defaultLightPrimaryMatch?.[1]?.trim() ?? "",
  dark: defaultDarkPrimaryMatch?.[1]?.trim() ?? "",
};

presets.unshift({ label: "デフォルト", value: "default", primary: defaultPrimary });

const generatedBlock = `// --- generated:themePresets:start ---

export const THEME_PRESET_OPTIONS = ${JSON.stringify(presets, null, 2)} as const;

export const THEME_PRESET_VALUES = THEME_PRESET_OPTIONS.map((p) => p.value);

export type ThemePreset = (typeof THEME_PRESET_OPTIONS)[number]["value"];

// --- generated:themePresets:end ---`;

const fileContent = fs.readFileSync(outputPath, "utf8");

const updated = fileContent.replace(
  /\/\/ --- generated:themePresets:start ---[\s\S]*?\/\/ --- generated:themePresets:end ---/,
  generatedBlock,
);

function main() {
  const biomeBin = require.resolve("@biomejs/biome/bin/biome");
  const formatted = execFileSync(process.execPath, [biomeBin, "format", "--stdin-file-path", outputPath], {
    input: updated,
    encoding: "utf8",
  });

  if (formatted === fileContent) {
    console.log("ℹ️ theme.ts に変更はありません");
    return;
  }

  fs.writeFileSync(outputPath, formatted);
  console.log("✅ theme.ts のプリセットを更新しました");
}

try {
  main();
} catch (err) {
  console.error("❌ テーマのプリセット生成中に予期しないエラーが発生しました：", err);
  process.exit(1);
}
