// Mock スイッチ。ビルド時に注入する公開変数（NEXT_PUBLIC_ 接頭辞のみブラウザーから読み取り可能）。
// Vercel で NEXT_PUBLIC_MOCK=1 にするとサイト全体で mock を使い、バックエンドは不要。
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
