import { fileURLToPath } from "node:url";

// 静的エクスポート：`NEXT_EXPORT=1 next build` で完全な静的ディレクトリを web/out に出力し、そのまま
// nginx web ルートディレクトリに置いて実行できる。開発(next dev)時はこの変数を設定せず、/api リバースプロキシとホット更新を保持する。
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo：サイト全体で mock を使い、バックエンドも /api リバースプロキシも不要。
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 親ディレクトリの lockfile がルートディレクトリの推定やリソースパス生成に影響しないようにする。
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // ローカルネットワーク IP から dev リソース（HMR）へのアクセスを許可し、必要に応じて追加・削除する。
  // dev 段階では任意の IPv4 送信元から /_next/* と HMR へのアクセスを許可する（ローカルネットワーク IP が変わっても影響しない）。
  // 注意：Next は安全上、単独の "*" を禁止するため、区切りごとのワイルドカードを使う；"*.*.*.*" は任意の IPv4 に一致する。
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 完全な静的エクスポート：Node ランタイムなし；画像を最適化しない；各ルートに <route>/index.html を出力する。
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo：バックエンドがなく、/api リバースプロキシは不要。
          images: { unoptimized: true },
        }
      : {
          // 開発：/api/* を Go バックエンドへリバースプロキシする（デフォルト :8787、AUTOPENTEST_API で上書き可能）。
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
