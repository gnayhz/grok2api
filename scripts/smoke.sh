#!/usr/bin/env bash
# smoke.sh — 部署后冒烟矩阵（optimize 分支 round 8 建立）。
# 用法: BASE=http://127.0.0.1:38000 ADMIN_USER=root ADMIN_PASS=... scripts/smoke.sh
# 全部通过输出 SMOKE: PASS；任一失败非零退出。
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:38000}"
ADMIN_USER="${ADMIN_USER:-root}"
ADMIN_PASS="${ADMIN_PASS:?need ADMIN_PASS}"

say() { printf '%-28s' "$1"; }
ok() { echo "ok"; }
fail() { echo "FAILED: $1"; exit 1; }

say "healthz"
curl -sf -m 5 "$BASE/healthz" | grep -q '"ok":true' && ok || fail "healthz not ok"

say "readyz"
code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "$BASE/readyz")
[ "$code" = "200" ] && ok || fail "readyz=$code"

say "admin login"
LOGIN_PAYLOAD=$(printf '{"username":"%s","password":"%s"}' "$ADMIN_USER" "$ADMIN_PASS")
TOKEN=$(curl -sf -m 5 -X POST "$BASE/api/admin/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "$LOGIN_PAYLOAD" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["tokens"]["accessToken"])') \
  && ok || fail "login rejected"

say "admin accounts"
curl -sf -m 5 -H "Authorization: Bearer $TOKEN" "$BASE/api/admin/v1/accounts?page=1&pageSize=1" | grep -q '"items"' && ok || fail "accounts API"

say "admin request-audits"
curl -sf -m 5 -H "Authorization: Bearer $TOKEN" "$BASE/api/admin/v1/request-audits?limit=1" | grep -q '"items"' && ok || fail "audits API"

say "client key lifecycle"
PAYLOAD=$(printf '{"name":"smoke-script","rpmLimit":60,"maxConcurrent":4,"enabled":true}')
KEYJSON=$(curl -sf -m 5 -X POST "$BASE/api/admin/v1/client-keys" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "$PAYLOAD")
SECRET=$(echo "$KEYJSON" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["secret"])')
KEYID=$(echo "$KEYJSON" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["key"]["id"])')
ok

say "invalid key rejected"
code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer invalid" "$BASE/v1/models" || true)
[ "$code" = "401" ] && ok || fail "invalid key got '$code'"

say "real inference (non-stream)"
INFER_PAYLOAD=$(printf '{"model":"grok-4.6","stream":false,"messages":[{"role":"user","content":"Reply with exactly: SMOKE-OK"}]}')
ANSWER=$(curl -sf -m 60 -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d "$INFER_PAYLOAD" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["choices"][0]["message"]["content"])') \
  && echo "$ANSWER" | grep -q "SMOKE-OK" && ok || fail "answer=$ANSWER"

say "real inference (stream + DONE)"
STREAM_PAYLOAD=$(printf '{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"Reply with exactly: STREAM-OK"}]}')
curl -s -m 90 -N -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d "$STREAM_PAYLOAD" \
  | grep -q '^data: \[DONE\]' && ok || fail "stream missing [DONE]"

say "cleanup smoke key"
DELETE_PAYLOAD=$(printf '{"ids":["%s"]}' "$KEYID")
curl -sf -m 5 -X DELETE "$BASE/api/admin/v1/client-keys" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "$DELETE_PAYLOAD" | grep -q '"deleted":1' && ok || fail "key cleanup"

echo "SMOKE: PASS"
