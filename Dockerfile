# syntax=docker/dockerfile:1
#
# 実行イメージ（イメージ内ではビルドしない）：一般的なツールだけを入れ、**ビルド済みの Linux 単一バイナリ**を配置する。
# バイナリは CI の binaries job でクロスコンパイルし（純粋な Go、QEMU なし）、対象アーキテクチャに合わせて
# ビルドコンテキストの dist/<TARGETARCH>/artex に置く。これにより複数アーキテクチャのビルド時、arm64 は apt 層だけをエミュレートし、
# Next/Go コンパイルをエミュレートしないため、はるかに高速になる。
#
# ローカルでイメージを手動ビルドする場合は、先にバイナリを用意する：
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# 一般的なツール：ripgrep / curl / vim と recon の常用ツール群（必要に応じて追加・削除）。
# Node は NodeSource から 20.x をインストール：bookworm 標準の apt nodejs は 18 だが、Playwright は >=20 が必要。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Playwright MCP と CLI を（グローバルに）事前インストールし、実行時の npx によるネットダウンロードを不要にする。
# @playwright/mcp：browser MCP は直接 `npx @playwright/mcp`（グローバルにインストール済みで、-y/@latest は不要）。
# @playwright/cli：playwright-cli を提供し、インストール後に --help で実行可能かも確認する。
# さらに playwright（ブラウザー管理用）をインストールし、--with-deps で chromium とそのシステム依存も事前配置することで、
# コンテナー内の MCP/CLI を初回起動から使え、ブラウザーのネットダウンロードも不要にする。
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# 対応アーキテクチャのビルド済みバイナリ（dist/amd64/artex または dist/arm64/artex）
COPY dist/${TARGETARCH}/artex /app/artex
# 監視起動スクリプト：プロセス終了後に終了コードで再起動の要否を決め、画面のワンクリック更新はこれで入れ替えを完了する。
# SIGTERM の artex への転送も担当する —— docker stop は PID 1 にしかシグナルを送らず、
# 転送しないと artex が受け取れず正常な終了処理ができないため、10 秒後に SIGKILL で強制終了される。
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/（SQLite + jwt.key）の永続化先
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
