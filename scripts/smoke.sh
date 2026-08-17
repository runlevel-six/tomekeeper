#!/usr/bin/env bash
#
# Acceptance test for the container image rather than the source.
#
#   1. `docker run` exits nonzero with a clear message when TOME_DATABASE_URL is unset
#   2. a configured server answers /healthz and /readyz
#   3. the web interface is served from inside the binary — no filesystem needed
#   4. the image runs as a non-root user
#   5. SIGTERM reaches the binary directly and produces a clean exit
#
# Most of these also exist at the Go level. They are repeated here because the
# thing being accepted is the *image*: an entrypoint typo, a stray `USER root`, a
# missing go:embed, or a shell wrapper swallowing signals would pass every unit
# test and fail in production.
#
# A real PostgreSQL is started alongside, because `tome serve` verifies the
# connection at startup and exits if it cannot reach one. An earlier version of
# this script pointed the DSN at 127.0.0.1 inside the container, where nothing is
# listening, so the server exited before answering anything — it could never have
# passed. It went unnoticed because the workflow that runs it was watching a
# branch that does not exist.
#
# Usage: scripts/smoke.sh [image-tag]

set -euo pipefail

IMAGE="${1:-tomekeeper:smoke}"
SUFFIX="$$"
CONTAINER="tomekeeper-smoke-$SUFFIX"
DB_CONTAINER="tomekeeper-smoke-db-$SUFFIX"
NETWORK="tomekeeper-smoke-net-$SUFFIX"
PORT="${SMOKE_PORT:-18080}"

# The database is reachable by container name on the user-defined network, which
# is why one is created rather than using the default bridge.
DSN="postgres://tome:smoke@${DB_CONTAINER}:5432/tome?sslmode=disable"

pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1" >&2; exit 1; }

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker rm -f "$DB_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v docker >/dev/null || fail "docker is not installed"
command -v curl >/dev/null || fail "curl is not installed"

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

echo "==> Starting PostgreSQL for the remaining cases"
docker network create "$NETWORK" >/dev/null
docker run -d --name "$DB_CONTAINER" --network "$NETWORK" \
  -e POSTGRES_USER=tome -e POSTGRES_PASSWORD=smoke -e POSTGRES_DB=tome \
  postgres:16-alpine >/dev/null

for _ in $(seq 1 60); do
  docker exec "$DB_CONTAINER" pg_isready -U tome >/dev/null 2>&1 && break
  sleep 0.5
done
docker exec "$DB_CONTAINER" pg_isready -U tome >/dev/null 2>&1 \
  || fail "PostgreSQL did not become ready"
pass "PostgreSQL is ready"

echo "==> Applying migrations from the image"
docker run --rm --network "$NETWORK" \
  -e "TOME_DATABASE_URL=$DSN" \
  -e "TOME_PASSWORD=smoke-password" \
  "$IMAGE" migrate >/dev/null \
  || fail "migrate failed against a reachable database"
pass "migrate succeeded"

echo "==> Case 2: a configured server answers its probes"
docker run -d --name "$CONTAINER" --network "$NETWORK" \
  -e "TOME_DATABASE_URL=$DSN" \
  -e "TOME_COOKIE_SECURE=false" \
  -p "127.0.0.1:${PORT}:8080" \
  "$IMAGE" >/dev/null

base="http://127.0.0.1:${PORT}"
for _ in $(seq 1 60); do
  curl -sf -o /dev/null "${base}/healthz" && break
  sleep 0.25
done

check_status() {
  local path="$1" want="$2"
  local got
  got=$(curl -s -o /dev/null -w '%{http_code}' "${base}${path}")
  if [ "$got" != "$want" ]; then
    docker logs "$CONTAINER" >&2
    fail "GET $path returned $got, want $want"
  fi
  pass "GET $path returned $got"
}

check_status /healthz 200
# 200 rather than 503 is the point: readiness consults the database, so this
# passing means the container genuinely reached PostgreSQL over the network.
check_status /readyz 200

echo "==> Case 3: the web interface is served from inside the binary"
# The templates and stylesheet are compiled in with go:embed. The runtime image is
# distroless — no shell, no package manager, and nothing on disk to fall back on —
# so this is the only place a missing embed would show up.
check_status /login 200

login=$(curl -s "${base}/login")
grep -q "Tomekeeper" <<<"$login" || fail "the sign-in page did not render: $login"
pass "the sign-in page rendered"

csp=$(curl -sI "${base}/login" | tr -d '\r' | grep -i '^content-security-policy:' || true)
[ -n "$csp" ] || fail "no Content-Security-Policy header on an HTML page"
grep -q "default-src 'none'" <<<"$csp" || fail "unexpected CSP: $csp"
pass "CSP header present"

check_status /static/tome.css 200
check_status /static/vendor/htmx-2.0.9.min.js 200
pass "embedded static assets are served"

# An unauthenticated request for a reading view must not serve the archive.
redirect=$(curl -s -o /dev/null -w '%{http_code}' "${base}/")
[ "$redirect" = "303" ] || fail "GET / while signed out returned $redirect, want 303"
pass "GET / while signed out redirects to sign-in"

echo "==> Case 4: the image runs as a non-root user"
# distroless/static:nonroot is uid 65532; a stray USER root in a future edit
# should break this.
user=$(docker inspect -f '{{.Config.User}}' "$IMAGE")
[ "$user" = "nonroot:nonroot" ] || fail "image runs as '$user', want nonroot:nonroot"
pass "image runs as $user"

echo "==> Case 5: SIGTERM produces a clean exit"
# SIGTERM must reach the binary itself (pid 1, no shell wrapper), or every rolling
# restart drops in-flight requests.
docker stop -t 20 "$CONTAINER" >/dev/null
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$CONTAINER")
[ "$exit_code" = "0" ] || fail "container exited $exit_code after SIGTERM, want 0"
pass "clean exit on SIGTERM"

echo "==> Container acceptance criteria met"
