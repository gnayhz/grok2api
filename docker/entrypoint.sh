#!/bin/sh
set -eu

umask 077

if [ ! -f "${GROK2API_CONFIG_SOURCE}" ]; then
  echo "missing config: ${GROK2API_CONFIG_SOURCE}" >&2
  echo "mount config.yaml to /run/grok2api/config.yaml" >&2
  exit 1
fi

cp "${GROK2API_CONFIG_SOURCE}" /app/config.yaml
chown grok2api:grok2api /app/config.yaml
chmod 0600 /app/config.yaml

# Derive GOMEMLIMIT from the container cgroup when the operator did not set it.
# Do not touch GOGC: lowering it trades TTFB p99 for a smaller heap.
if [ -z "${GOMEMLIMIT:-}" ]; then
  limit=""
  if [ -r /sys/fs/cgroup/memory.max ]; then
    limit=$(cat /sys/fs/cgroup/memory.max)
  elif [ -r /sys/fs/cgroup/memory/memory.limit_in_bytes ]; then
    limit=$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes)
  fi
  case "${limit}" in
    ""|max) ;;
    *[!0-9]*) ;;
    *)
      # Ignore cgroup v1 "unlimited" (near 2^63) and tiny limits.
      if [ "${limit}" -ge 67108864 ] && [ "${limit}" -lt 1099511627776 ]; then
        GOMEMLIMIT=$((limit * 90 / 100))
        export GOMEMLIMIT
      fi
      ;;
  esac
fi

exec su-exec grok2api:grok2api "$@"
