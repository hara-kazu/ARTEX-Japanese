#!/usr/bin/env bash
# ARTEX の更新スクリプト：① Docker 更新（新しいイメージを取得して再作成）  ② ローカルの実行ファイルを再ビルド
# install.sh に対応：install は初回導入、update は新バージョンへの更新を担当する。
# DB 移行の手動実行は不要——artex は起動のたびに schema.sql（ADD COLUMN/CREATE
# INDEX IF NOT EXISTS を含む）を冪等に再実行するため、“再起動で移行”される。データ（pgdata ボリューム、./data、./skills）は影響を受けない。
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── 任意：リポジトリを最新コードへ同期（compose/スクリプト/ローカルビルドのソースはこれで更新）───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "git の作業コピーではないため、git pull をスキップします"; return; }
  [ "$(ask '最新のソースを取得しますか（git pull --ff-only）？ (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull を早送りできません（ローカルの変更またはブランチの分岐あり）。手動で解決して再試行してください。今回は現在のソースを使用します"
  fi
}

# ── ① Docker 更新 ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose がありません。先に ./install.sh で導入してください"
  [ -f .env ] || die ".env がありません。先に ./install.sh で初回の導入を行ってください"

  # 任意：指定バージョンの tag に更新（未入力なら .env の ARTEX_TAG を使い、省略時は latest）
  local tag; tag="$(ask '対象イメージの tag（エンターで .env / latest を使用）' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG を ${tag} に設定しました"
  fi

  # artex だけを変更：postgres は 16-alpine 固定で、同時更新は不要（取得は帯域の無駄で、
  # メジャーバージョン変更には互換性リスクもある）。artex は depends_on postgres を宣言し、サービス名付きの
  # up では pg が未起動なら自動起動し、起動中ならそのまま保持して再作成しない。
  info "新しいイメージを取得します（artex のみ）…"
  docker compose pull artex
  info "再作成して起動します（artex の再起動時に schema を自動移行）…"
  docker compose up -d artex
  ok "更新完了 → http://localhost:8787"
  info "ログを表示：docker compose logs -f artex"
  info "古いイメージを削除（任意）：docker image prune -f"
}

# ── ② ローカルでビルドして更新 ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go が見つかりません（>=1.26）：https://go.dev/dl/"
  [ -f config.json ] || warn "config.json がありません。初回の導入には ./install.sh を使用してください"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "フロントエンドの静的ファイルを再ビルドします…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "フロントエンドを埋め込んだ実行ファイルを再ビルドします…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm がありません。**フロントエンドを含まない**バックエンドをビルドします（別途 npm run dev が必要）"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "ビルド完了 → ./artex"
  warn "実行中の artex プロセスを再起動して反映してください（再起動時に schema を自動移行）"
}

echo "=============================="
echo "  ARTEX の更新"
echo "  1) Docker 更新（イメージを取得して再作成）"
echo "  2) ローカル更新（go で再ビルド）"
echo "=============================="
case "$(ask '選択' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "無効な選択です" ;;
esac
