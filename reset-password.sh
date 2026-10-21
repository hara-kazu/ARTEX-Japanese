#!/usr/bin/env bash
# =============================================================================
# ARTEX 管理员密码重置脚本
#
# 登录用户名固定为 ARTEX；密码以 bcrypt 哈希存放在数据库 settings 表的
# auth.password_hash 键。本脚本连上数据库后，用 pgcrypto 在库内生成 bcrypt 哈希
# 并写回该键——与后端登录校验（golang.org/x/crypto/bcrypt）完全兼容。
#
# 两种部署：
#   local （默认）—— 宿主机直接用 psql 连数据库。连接信息按以下优先级获取：
#                    命令行参数 > --dsn/$ARTEX_PG_DSN > config.json 的 database.*
#   docker        —— 通过 `docker compose exec`（或 `docker exec`）在 postgres
#                    容器内执行 psql（compose 默认不对宿主暴露 5432，故走容器内）。
#
# 用法示例：
#   ./reset-password.sh                          # 本地，自动读 config.json/环境，交互输入新密码
#   ./reset-password.sh -p 'NewPass!'            # 本地，直接给定新密码
#   ./reset-password.sh --dsn postgres://u:p@h:5432/artex
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U autopentest -W pass -d artex
#   ./reset-password.sh -m docker                # docker 部署（读 .env 的 POSTGRES_*）
#   ./reset-password.sh -m docker -c pg容器名 --exec docker
#
# 安全：新密码经环境变量 + psql \getenv 传入（不进入进程 argv），并用 :'var'
# 自动转义（防 SQL 注入）；数据库密码经 PGPASSWORD 传递，同样不进 argv。
# =============================================================================
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker（空=自动判定）
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # docker 模式的 postgres 服务/容器名（默认 postgres）
EXEC_KIND=""       # compose | docker（docker 模式下用哪种 exec；空=自动）
NEWPASS=""
ASSUME_YES=0

die() { echo "エラー：$*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() {
  cat <<'EOF'
ARTEX 管理者パスワードのリセット

使用方法：./reset-password.sh [オプション]
  -m, --mode local|docker   接続方法（省略時は自動判定）
  --dsn DSN                PostgreSQL の接続文字列
  -H, --host HOST          データベースのホスト
  -P, --port PORT          データベースのポート
  -U, --user USER          データベースのユーザー
  -W, --db-password PASS   データベースのパスワード
  -d, --dbname NAME        データベース名
  --sslmode MODE           PostgreSQL の SSL モード
  --config PATH            設定ファイル（標準：config.json）
  -c, --container NAME     Docker のサービス / コンテナー名
  --exec compose|docker    Docker の実行方法
  -p, --new-password PASS  新しいパスワード（省略時は非表示で対話入力）
  -y, --yes                最終確認を省略
  -h, --help               このヘルプを表示

接続設定の優先度：コマンド引数 > ARTEX_PG_DSN > config.json。
ログイン名は ARTEX 固定です。反映後のサービス再起動は不要です。
EOF
  exit 0
}

# ---- 参数解析 -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "不明な引数：$1（-h で使用方法を確認）" ;;
  esac
done

# ---- 从 config.json 读取 database.*（仅 local 模式、且未显式给出连接时）-----
# 优先用 python3 解析（健壮）；缺 python3 时退回 grep（config.json 为规整分字段）。
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# 支持直接给 dsn，或分字段
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # 极简后备：逐键 grep（值为字符串或数字）
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- 自动判定模式 ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${ARTEX_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "導入モード：$MODE"

# ---- 采集新密码 -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "新しいパスワードを入力（ログイン名は ARTEX）：" NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "パスワードを入力してください"
  read -r -s -p "確認のため再入力：" NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "入力したパスワードが一致しません"
fi
[[ -n "$NEWPASS" ]] || die "パスワードを入力してください"

# 通过环境变量把密码交给 psql（\getenv 读取，不进入 argv/ps）
export ARTEX_RESET_NEWPASS="$NEWPASS"

# 库内生成 bcrypt 并 upsert；密码用 :'newpw' 自动转义。CREATE EXTENSION 幂等，
# 若数据库角色无建扩展权限会在此报错（提示见下方 run 的失败分支）。
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- 执行 -----------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # 连接信息优先级：命令行 > --dsn/$ARTEX_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "$cfg からデータベース設定を読み込みます"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "psql がありません（postgresql-client をインストールするか、-m docker を使用してください）"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "データベースユーザー（-U）または有効な config.json / DSN が必要です"
    [[ -n "$DBNAME" ]] || die "データベース名（-d）または有効な config.json / DSN が必要です"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "対象データベース：$target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "このデータベースの ARTEX パスワードをリセットしますか？[y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "キャンセルしました"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "保存に失敗しました。pgcrypto がない、または権限エラーの場合は、拡張機能を作成できるロールを使用するか、先に CREATE EXTENSION pgcrypto を実行してください。"
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "Docker が見つかりません"
  CONTAINER="${CONTAINER:-postgres}"

  # 选择 exec 方式：优先 docker compose exec（服务名），否则 docker exec（容器名）
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # 容器内的 psql 凭据：优先命令行，其次 .env 的 POSTGRES_*，再退回 compose 默认(artex)
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-artex}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-artex}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "対象：コンテナー $CONTAINER の psql -U $DUSER -d $DNAME（exec=$EXEC_KIND）"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "このコンテナーのデータベースで ARTEX パスワードをリセットしますか？[y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "キャンセルしました"
  fi

  # -e 只带名字不带值 → 从当前环境继承，密码不出现在 docker 命令 argv 里。
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "保存に失敗しました。コンテナー名（-c）、データベースの認証情報（.env の POSTGRES_*）、pgcrypto の権限を確認してください。"
  fi
fi

unset ARTEX_RESET_NEWPASS
echo "✓ ARTEX 管理者のパスワードをリセットしました。ARTEX と新しいパスワードでログインしてください（再起動は不要です）。"
