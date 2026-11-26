#!/usr/bin/env sh
# Prepare approved migrations and run the k6 stress test against the running
# Docker Compose stack.
#
# Usage:
#   docker compose up -d --build
#   docker compose run --rm --entrypoint /seed app
#   ./scripts/k6.sh
set -eu

cd "$(dirname "$0")/.."

if [ -f .env ]; then
    set -a
    # shellcheck disable=SC1091
    . ./.env
    set +a
fi

NETWORK="${COMPOSE_PROJECT_NAME:-if3110_k03-06}_default"
BASE_URL="${BASE_URL:-http://app:8080}"
POOL_SIZE="${K6_MIGRATION_POOL:-150}"

echo "1/2 Seeding $POOL_SIZE pre-approved migrations per database into k6/migrations.json..."
docker run --rm --network "$NETWORK" \
    -v "$PWD":/src -w /src \
    -v if3110-gomod:/go/pkg/mod -v if3110-gocache:/root/.cache/go-build \
    -e DATABASE_URL="postgres://platform:${PLATFORM_DB_PASSWORD:?Set PLATFORM_DB_PASSWORD in .env}@platform-db:5432/platform?sslmode=disable" \
    -e DB_CREDENTIAL_KEY="${DB_CREDENTIAL_KEY:?Set DB_CREDENTIAL_KEY in .env}" \
    -e BOOTSTRAP_ADMIN_USERNAME="${BOOTSTRAP_ADMIN_USERNAME:-admin}" \
    -e K6_MIGRATION_POOL="$POOL_SIZE" \
    -e K6_MANIFEST_PATH="/src/k6/migrations.json" \
    golang:1.26-alpine go run ./k6/seed

echo "2/2 Running k6 stress test against $BASE_URL..."
docker run --rm --network "$NETWORK" \
    -v "$PWD":/work -w /work \
    -e BASE_URL="$BASE_URL" \
    -e K6_USERNAME="${BOOTSTRAP_ADMIN_USERNAME:-admin}" \
    -e K6_PASSWORD="${BOOTSTRAP_ADMIN_PASSWORD:?Set BOOTSTRAP_ADMIN_PASSWORD in .env}" \
    -e K6_MANIFEST_PATH="/work/k6/migrations.json" \
    grafana/k6:latest run /work/k6/stress.js "$@"
