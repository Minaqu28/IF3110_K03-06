#!/usr/bin/env sh
#   ./scripts/test.sh              unit tests only
#   ./scripts/test.sh -i           unit + integration tests (starts a throwaway Postgres)
#   ./scripts/test.sh -i -run Target   extra args are passed to `go test`
set -eu

cd "$(dirname "$0")/.."

INTEGRATION=0
if [ "${1:-}" = "-i" ]; then
    INTEGRATION=1
    shift
fi

NET=if3110-test-net
PG=if3110-test-pg
GO_IMAGE=golang:1.26-alpine
DSN=""

cleanup() {
    docker rm -f "$PG" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
}

if [ "$INTEGRATION" = 1 ]; then
    trap cleanup EXIT INT TERM
    cleanup
    docker network create "$NET" >/dev/null
    docker run -d --name "$PG" --network "$NET" -e POSTGRES_PASSWORD=test postgres:17-alpine >/dev/null
    echo "waiting for postgres..."
    until docker exec "$PG" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
    sleep 1
    DSN="postgres://postgres:test@$PG:5432/postgres?sslmode=disable"
fi

NETWORK_ARGS=""
[ "$INTEGRATION" = 1 ] && NETWORK_ARGS="--network $NET"

# shellcheck disable=SC2086
docker run --rm $NETWORK_ARGS \
    -v "$PWD":/src -w /src \
    -v if3110-gomod:/go/pkg/mod -v if3110-gocache:/root/.cache/go-build \
    -e TEST_DATABASE_URL="$DSN" \
    -e TEST_POSTGRES_DSN="$DSN" \
    "$GO_IMAGE" go test -count=1 "$@" ./...
