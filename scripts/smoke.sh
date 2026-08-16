#!/usr/bin/env bash
#
# M0 acceptance test, run against the container image rather than the source.
#
#   1. `docker run` starts a server that returns 200 on /healthz
#   2. it exits nonzero with a clear message when TOME_DATABASE_URL is unset
#
# Both assertions also exist at the Go level (cmd/tome/main_test.go and
# internal/server/server_test.go). They are repeated here because the thing
# being accepted is the image: an entrypoint typo or a nonroot user that cannot
# bind the port would pass every unit test and fail in production.
#
# Usage: scripts/smoke.sh [image-tag]

set -euo pipefail

IMAGE="${1:-tomekeeper:smoke}"
CONTAINER="tomekeeper-smoke-$$"
PORT="${SMOKE_PORT:-18080}"
DSN="postgres://tome:smoke@127.0.0.1:5432/tome?sslmode=disable"

pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; exit 1; }

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

command -v docker >/dev/null || fail "docker is not installed"

echo "==> Building $IMAGE"
docker build \
  --build-arg "VERSION=${VERSION:-smoke}" \
  --build-arg "COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
  -t "$IMAGE" .

echo "==> Case 1: missing TOME_DATABASE_URL must fail loudly"
set +e
output=$(docker run --rm "$IMAGE" serve 2>&1)
code=$?
set -e

[ "$code" -ne 0 ] || fail "exited 0 with no TOME_DATABASE_URL, want nonzero"
pass "exited $code"

grep -q "TOME_DATABASE_URL" <<<"$output" \
  || fail "the error message does not name TOME_DATABASE_URL: $output"
pass "message names the missing variable"

grep -qi "required" <<<"$output" \
  || fail "the error message does not say the value is required: $output"
pass "message says what is wrong"

echo "==> Case 2: a configured server answers /healthz"
docker run -d --name "$CONTAINER" \
  -e "TOME_DATABASE_URL=$DSN" \
  -p "127.0.0.1:${PORT}:8080" \
  "$IMAGE" >/dev/null

for _ in $(seq 1 50); do
  curl -sf -o /dev/null "http://127.0.0.1:${PORT}/healthz" && break
  sleep 0.2
done

status=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/healthz")
[ "$status" = "200" ] || {
  docker logs "$CONTAINER" >&2
  fail "GET /healthz returned $status, want 200"
}
pass "GET /healthz returned 200"

status=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/readyz")
[ "$status" = "200" ] || fail "GET /readyz returned $status, want 200"
pass "GET /readyz returned 200"

# The container must run as a non-root user. distroless/static:nonroot is uid
# 65532; a stray USER root in a future edit should break this test.
uid=$(docker inspect -f '{{.Config.User}}' "$IMAGE")
[ "$uid" = "nonroot:nonroot" ] || fail "image runs as '$uid', want nonroot:nonroot"
pass "image runs as $uid"

# SIGTERM must be received by the binary itself (pid 1, no shell wrapper) and
# produce a clean exit, or every rolling restart drops in-flight requests.
docker stop -t 20 "$CONTAINER" >/dev/null
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$CONTAINER")
[ "$exit_code" = "0" ] || fail "container exited $exit_code after SIGTERM, want 0"
pass "clean exit on SIGTERM"

echo "==> M0 acceptance criteria met"
