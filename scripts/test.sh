#!/usr/bin/env sh
#   ./scripts/test.sh              unit tests only
#   ./scripts/test.sh -i           unit + integration tests (starts a throwaway Postgres and RabbitMQ)
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
MQ=if3110-test-mq
GO_IMAGE=golang:1.26-alpine
DSN=""
AMQP=""

cleanup() {
    docker rm -f "$PG" "$MQ" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
}

if [ "$INTEGRATION" = 1 ]; then
    trap cleanup EXIT INT TERM
    cleanup
    docker network create "$NET" >/dev/null
    docker run -d --name "$PG" --network "$NET" -e POSTGRES_PASSWORD=test postgres:17-alpine >/dev/null
    docker run -d --name "$MQ" --network "$NET" -e RABBITMQ_DEFAULT_USER=test -e RABBITMQ_DEFAULT_PASS=test rabbitmq:4-alpine >/dev/null
    echo "waiting for postgres..."
    until docker exec "$PG" pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
    sleep 1
    DSN="postgres://postgres:test@$PG:5432/postgres?sslmode=disable"
    echo "waiting for rabbitmq..."
    until docker exec "$MQ" rabbitmq-diagnostics -q check_port_connectivity >/dev/null 2>&1; do sleep 1; done
    AMQP="amqp://test:test@$MQ:5672/"
fi

NETWORK_ARGS=""
[ "$INTEGRATION" = 1 ] && NETWORK_ARGS="--network $NET"

# shellcheck disable=SC2086
docker run --rm $NETWORK_ARGS \
    -v "$PWD":/src -w /src \
    -v if3110-gomod:/go/pkg/mod -v if3110-gocache:/root/.cache/go-build \
    -e TEST_DATABASE_URL="$DSN" \
    -e TEST_POSTGRES_DSN="$DSN" \
    -e TEST_AMQP_URL="$AMQP" \
    "$GO_IMAGE" go test -count=1 "$@" ./...
