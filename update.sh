#!/usr/bin/env bash
# ARTEX の更新スクリプト：① Docker のイメージを更新  ② ローカルの実行ファイルを再ビルド
# 与 install.sh 对应：install 负责首次落地，update 负责升级到新版本。
# DB 迁移无需手动执行——artex 每次启动都会幂等重跑 schema.sql（含 ADD COLUMN/CREATE
# INDEX IF NOT EXISTS），所以“重启即迁移”。数据（pgdata 卷、./data、./skills）不受影响。
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── 可选：同步仓库到最新代码（compose/脚本/本地编译源码都靠它更新）───────
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

  # 可选：升级到指定版本 tag（不填则沿用 .env 中的 ARTEX_TAG，缺省为 latest）
  local tag; tag="$(ask '対象イメージの tag（エンターで .env / latest を使用）' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG を ${tag} に設定しました"
  fi

  # 只动 artex：postgres 是固定的 16-alpine，不需要跟着升级（拉它纯属浪费带宽，
  # 且大版本变动还会有兼容风险）。artex 声明了 depends_on postgres，所以带服务名
  # up 时若 pg 没起会自动拉起，已在跑的则原样保留、不重建。
  info "新しいイメージを取得します（artex のみ）…"
  docker compose pull artex
  info "再作成して起動します（artex の再起動時に schema を自動移行）…"
  docker compose up -d artex
  ok "更新完了 → http://localhost:8787"
  info "ログを表示：docker compose logs -f artex"
  info "古いイメージを削除（任意）：docker image prune -f"
}

# ── ② 本地编译更新 ──────────────────────────────
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
    warn "npm がありません。フロントエンドを含まないバックエンドをビルドします（別途 npm run dev が必要）"
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
