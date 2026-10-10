#!/usr/bin/env bash
# Runs two SFU nodes on this machine as one cluster (ports 8081 and 8082),
# sharing PostgreSQL and Valkey. Stop with Ctrl+C.
#
#   scripts/run-cluster.sh
#
# Needs PostgreSQL on localhost:5432 and Valkey/Redis on localhost:6379
# (see docker-compose.yml). Uses Redis database 1, so it does not mix with a
# single dev server on database 0.
set -euo pipefail
cd "$(dirname "$0")/.."

export SFU_DATABASE_URL="${SFU_DATABASE_URL:-postgres://sfu:sfu@localhost:5432/sfu}"
export SFU_REDIS_URL="${SFU_REDIS_URL:-redis://localhost:6379/1}"
export SFU_ADMIN_SECRET="${SFU_ADMIN_SECRET:-change-me-admin-secret-at-least-32-chars}"

go build -o /tmp/sfu-cluster ./cmd/sfu
/tmp/sfu-cluster -addr :8081 -node-id node-1 -node-url http://127.0.0.1:8081 -stun "" -dev -drain-timeout 30s &
P1=$!
/tmp/sfu-cluster -addr :8082 -node-id node-2 -node-url http://127.0.0.1:8082 -stun "" -dev -drain-timeout 30s &
P2=$!
trap 'kill -INT $P1 $P2 2>/dev/null; wait' INT TERM
echo "node-1: http://localhost:8081   node-2: http://localhost:8082   (Ctrl+C stops both)"
wait
