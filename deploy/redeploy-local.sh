#!/usr/bin/env bash
# 本地源码一键重部署脚本。
# 目标：
# 1. 备份当前部署目录与数据库
# 2. 用当前源码构建本地镜像
# 3. 仅重建 sub2api 应用容器，不重建 postgres/redis，不删除任何数据卷

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [ -z "${DEPLOY_DIR:-}" ]; then
  if [ -d /root/sub2api-deploy ]; then
    DEPLOY_DIR="/root/sub2api-deploy"
  else
    DEPLOY_DIR="/Users/caolin/Desktop/projects/sub2api-deploy"
  fi
fi
COMPOSE_FILE="${COMPOSE_FILE:-${DEPLOY_DIR}/docker-compose.local.yml}"
ENV_FILE="${ENV_FILE:-${DEPLOY_DIR}/.env}"
IMAGE_TAG="${IMAGE_TAG:-sub2api:local}"
BACKUP_DIR="${BACKUP_DIR:-${DEPLOY_DIR}/backups}"
BUILD_GOPROXY="${BUILD_GOPROXY:-https://goproxy.cn,direct}"
BUILD_GOSUMDB="${BUILD_GOSUMDB:-sum.golang.google.cn}"

log() {
  printf '[redeploy] %s\n' "$*"
}

fail() {
  printf '[redeploy] 错误: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少命令: $1"
}

ensure_file() {
  local path="$1"
  [ -f "$path" ] || fail "文件不存在: $path"
}

timestamp() {
  date +%Y%m%d-%H%M%S
}

backup_deploy_dir() {
  local ts archive_path
  ts="$(timestamp)"
  mkdir -p "$BACKUP_DIR"
  archive_path="${BACKUP_DIR}/sub2api-deploy-${ts}.tar.gz"
  tar -czf "$archive_path" \
    --exclude='sub2api-deploy/backups' \
    -C "$(dirname "$DEPLOY_DIR")" \
    "$(basename "$DEPLOY_DIR")"
  printf '%s\n' "$archive_path"
}

read_env_value() {
  local key="$1"
  awk -F= -v target="$key" '$1 == target { sub(/^[^=]*=/, "", $0); print $0; exit }' "$ENV_FILE"
}

backup_database() {
  local postgres_container pg_user pg_password pg_db ts dump_path

  postgres_container="${POSTGRES_CONTAINER_NAME:-sub2api-postgres}"
  if ! docker ps --format '{{.Names}}' | grep -qx "$postgres_container"; then
    log "未发现数据库容器 ${postgres_container}，跳过数据库逻辑备份"
    return 0
  fi

  pg_user="${POSTGRES_USER:-$(read_env_value POSTGRES_USER)}"
  pg_password="${POSTGRES_PASSWORD:-$(read_env_value POSTGRES_PASSWORD)}"
  pg_db="${POSTGRES_DB:-$(read_env_value POSTGRES_DB)}"

  [ -n "$pg_user" ] || fail "无法读取 POSTGRES_USER"
  [ -n "$pg_password" ] || fail "无法读取 POSTGRES_PASSWORD"
  [ -n "$pg_db" ] || fail "无法读取 POSTGRES_DB"

  mkdir -p "$BACKUP_DIR"
  ts="$(timestamp)"
  dump_path="${BACKUP_DIR}/sub2api-db-${ts}.sql"

  PGPASSWORD="$pg_password" docker exec -e PGPASSWORD="$pg_password" "$postgres_container" \
    pg_dump -U "$pg_user" -d "$pg_db" -Fp > "$dump_path"

  printf '%s\n' "$dump_path"
}

build_image() {
  log "构建镜像 ${IMAGE_TAG}"
  docker build -t "$IMAGE_TAG" \
    --build-arg GOPROXY="$BUILD_GOPROXY" \
    --build-arg GOSUMDB="$BUILD_GOSUMDB" \
    -f "${REPO_ROOT}/Dockerfile" \
    "$REPO_ROOT"
}

deploy_app() {
  local override_file
  override_file="$(mktemp "${TMPDIR:-/tmp}/sub2api-redeploy-override.XXXXXX.yml")"
  cat > "$override_file" <<EOF
services:
  sub2api:
    image: ${IMAGE_TAG}
EOF

  log "重建应用容器（仅 sub2api）"
  docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" -f "$override_file" up -d --no-deps --force-recreate sub2api
  rm -f "$override_file"
}

wait_for_health() {
  local container_name health_status attempt
  container_name="${APP_CONTAINER_NAME:-sub2api}"

  log "等待容器健康检查通过"
  for attempt in 1 2 3 4 5 6 7 8 9 10 11 12; do
    health_status="$(docker inspect "$container_name" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' 2>/dev/null || true)"
    if [ "$health_status" = "healthy" ] || [ "$health_status" = "no-healthcheck" ]; then
      log "容器状态: ${health_status}"
      return 0
    fi
    sleep 5
  done

  log "容器未在预期时间内变为 healthy，输出最近日志供排查"
  docker logs --tail 120 "$container_name" || true
  return 1
}

show_summary() {
  local container_name
  container_name="${APP_CONTAINER_NAME:-sub2api}"

  log "当前容器状态"
  docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}' | grep -E '(^NAMES|sub2api)'
  log "最近日志"
  docker logs --tail 40 "$container_name" || true
}

main() {
  require_cmd docker
  require_cmd tar
  require_cmd awk

  ensure_file "$COMPOSE_FILE"
  ensure_file "$ENV_FILE"

  log "部署目录: ${DEPLOY_DIR}"
  log "Compose 文件: ${COMPOSE_FILE}"
  log "环境文件: ${ENV_FILE}"

  local deploy_backup db_backup
  deploy_backup="$(backup_deploy_dir)"
  log "部署目录备份完成: ${deploy_backup}"

  db_backup="$(backup_database || true)"
  if [ -n "${db_backup:-}" ]; then
    log "数据库备份完成: ${db_backup}"
  fi

  build_image
  deploy_app
  wait_for_health
  show_summary

  log "重部署完成"
}

main "$@"
