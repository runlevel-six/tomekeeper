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

# On failure, dump everything a reader would otherwise have to go and get: the
# server's log, the database's log, and the container states. A CI failure that
# says only "GET /healthz returned 000" costs a round trip to diagnose.
fail() {
  printf '  FAIL %s\n' "$1" >&2
  {
    echo
    echo "---- docker ps -a ----"
    docker ps -a --filter "name=tomekeeper-smoke" --format '{{.Names}}\t{{.Status}}\t{{.Image}}' || true
    for c in "$CONTAINER" "$DB_CONTAINER"; do
      if docker inspect "$c" >/dev/null 2>&1; then
        echo
        echo "---- logs: $c ----"
        docker logs --tail 60 "$c" 2>&1 || true
      fi
    done
  } >&2
  exit 1
}

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

[ "$code" -ne 0 ] || fail "exited 0 with no TOME_DATABASE_URL, want nonzero. Output: $output"
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

# Over TCP, and with a real query, deliberately.
#
# The official image initializes by starting a *temporary* server, creating the
# database, and shutting it down again before starting the real one. That
# temporary server listens on the unix socket only, so `pg_isready` with no host
# reports ready in the middle of initialization — this loop broke out of the wait
# during that window and then failed its own verification 200ms later, as the
# temporary server was being shut down. It looked like a 30-second timeout that
# had actually taken two seconds.
#
# Checking over TCP avoids it because listen_addresses is empty until the real
# server starts. The SELECT is belt and braces: it is the thing the next step
# actually needs to work, so it is the honest thing to wait for.
# PGPASSWORD comes from the container's own environment rather than being
# repeated here: over TCP the image authenticates with scram, so without it psql
# would sit waiting for a password nobody is going to type.
db_ready() {
  docker exec "$DB_CONTAINER" pg_isready -h 127.0.0.1 -U tome -d tome >/dev/null 2>&1 &&
    docker exec "$DB_CONTAINER" sh -c \
      'PGPASSWORD="$POSTGRES_PASSWORD" psql -w -h 127.0.0.1 -U tome -d tome -tAc "SELECT 1"' \
      >/dev/null 2>&1
}

ready=false
for _ in $(seq 1 60); do
  if db_ready; then ready=true; break; fi
  sleep 0.5
done
if [ "$ready" != true ]; then
  fail "PostgreSQL did not become ready within 30s"
fi
pass "PostgreSQL is ready"

echo "==> Applying migrations from the image"
if ! migrate_out=$(docker run --rm --network "$NETWORK" \
  -e "TOME_DATABASE_URL=$DSN" \
  -e "TOME_PASSWORD=smoke-password" \
  "$IMAGE" migrate 2>&1); then
  printf '%s\n' "$migrate_out" >&2
  fail "migrate failed against a reachable database"
fi
pass "migrate succeeded"

echo "==> Case 2: a configured server answers its probes"
docker run -d --name "$CONTAINER" --network "$NETWORK" \
  -e "TOME_DATABASE_URL=$DSN" \
  -e "TOME_COOKIE_SECURE=false" \
  -p "127.0.0.1:${PORT}:8080" \
  "$IMAGE" >/dev/null

base="http://127.0.0.1:${PORT}"
up=false
for _ in $(seq 1 60); do
  if curl -sf -o /dev/null "${base}/healthz"; then up=true; break; fi
  # A container that has already exited will never answer, so stop waiting for
  # it and report the reason it exited instead of timing out silently.
  if [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" != "true" ]; then
    fail "the server container exited before answering /healthz"
  fi
  sleep 0.25
done
[ "$up" = true ] || fail "the server did not answer /healthz within 15s"

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

# The vendored fonts, and specifically their Content-Type, which is the one thing
# here that cannot be proved anywhere else. Go's mime package has no builtin entry
# for woff2: it reads /etc/mime.types, which every developer machine has and this
# image does not. Without the header the server sets by hand, the file server would
# sniff the bytes, answer application/octet-stream, and serve that under nosniff —
# passing every test on a laptop and rendering the deployed archive in the fallback
# serif. This is the assertion that would catch it.
font=/static/vendor/fonts/literata-5.3.0-latin-wght-normal.woff2
check_status "$font" 200
font_headers=$(curl -sI "${base}${font}" | tr -d '\r')
grep -qi '^content-type: font/woff2' <<<"$font_headers" ||
  fail "the font was not served as font/woff2: $font_headers"
grep -qi '^cache-control:.*immutable' <<<"$font_headers" ||
  fail "the font is not cached immutably, so 320KB is revalidated forever: $font_headers"
pass "embedded fonts are served as woff2 and cached immutably"

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
