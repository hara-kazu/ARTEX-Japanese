#!/usr/bin/env bash
# ARTEX インストールスクリプト：① すべて Docker  ② ローカルでビルドして実行
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── docker 環境の検出 / 自動インストール ───────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker と docker compose を検出しました"; return
  fi
  warn "docker / docker compose が見つかりません"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Docker を自動インストールしますか？ (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker をインストールしました（ユーザーグループの変更後、sudo なしで使用するには再ログインが必要です）"
      else
        die "docker をインストールして再試行してください"
      fi ;;
    Darwin) die "macOS では Docker Desktop をインストールしてください：https://www.docker.com/products/docker-desktop/" ;;
    *)      die "docker をインストールして再試行してください" ;;
  esac
}

# ── ① すべて Docker ───────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres のパスワード（エンターでランダム生成）' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY（空欄にして後で UI から設定可能）' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env を作成しました（POSTGRES_PASSWORD 設定済み）"
  else
    info "既存の .env を使用します"
  fi
  info "イメージを取得して起動します…"
  docker compose pull || true
  docker compose up -d
  ok "起動しました → http://localhost:8787"
  info "ログを表示：docker compose logs -f artex"
}

# ── ② ローカルでビルドして実行 ──────────────────────────────
install_local(){
  echo "データベースの導入方法："
  echo "  1) 既存の PostgreSQL に接続"
  echo "  2) Docker で PostgreSQL を起動（docker が必要）"
  case "$(ask '選択' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres のパスワード（エンターでランダム生成）' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'データベースのアドレス' 127.0.0.1)"
      DB_PORT="$(ask 'ポート' 5432)"
      DB_USER="$(ask 'アカウント' artex)"
      DB_PASS="$(ask 'パスワード' '')"
      DB_NAME="$(ask 'データベース名' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # config.json を生成
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json を作成しました"

  # go 環境の確認
  command -v go >/dev/null 2>&1 || die "Go がありません。Go 1.26 以降をインストールしてください：https://go.dev/dl/"
  ok "Go: $(go version)"

  # フロントエンドの埋め込みには node で静的ファイルの生成が必要
  if command -v npm >/dev/null 2>&1; then
    info "フロントエンドの静的ファイルをビルドします…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "フロントエンドを埋め込んだ実行ファイルをビルドします…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm がありません。**フロントエンドを含まない**バックエンドをビルドします（別途 npm run dev が必要）"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "ビルド完了 → ./artex"

  info "起動します…（Ctrl-C で終了）"
  ./artex
}

echo "=============================="
echo "  ARTEX のインストール"
echo "  1) Docker で導入"
echo "  2) ローカルで実行（go でビルド）"
echo "=============================="
case "$(ask '選択' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "無効な選択です" ;;
esac
