#!/usr/bin/env bash
# 真实 PostgreSQL 迁移矩阵验证(EGRESS-REVIEW-STATUS.md 的可复现命令)。
# 起临时 postgres:16 容器, 跑 persistence/relational 全套集成测试
# (含 TestPostgresEgressLegacySchemaUpgrade 旧库升级), 结束自动清理。
# 用法: scripts/verify-postgres-migrations.sh [镜像tag, 默认 16-alpine]
set -euo pipefail

TAG="${1:-16-alpine}"
CONTAINER="grok2api-pg-verify-$$"
PORT="$((20000 + RANDOM % 20000))"

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo ">> starting postgres:$TAG on 127.0.0.1:$PORT"
docker run -d --rm --name "$CONTAINER" \
  -e POSTGRES_PASSWORD=verify -e POSTGRES_USER=verify \
  -p "127.0.0.1:$PORT:5432" "postgres:$TAG" >/dev/null

for _ in $(seq 1 60); do
  docker exec "$CONTAINER" pg_isready -U verify >/dev/null 2>&1 && break
  sleep 1
done

cd "$(dirname "$0")/../backend"
TEST_POSTGRES_ADMIN_DSN="postgres://verify:verify@127.0.0.1:$PORT/verify?sslmode=disable" \
  go test ./internal/infra/persistence/relational/ -count=1 -timeout 600s

echo ">> postgres:$TAG migration matrix verification passed"
