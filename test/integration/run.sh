#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
export BACKEND_CONTEXT="$ROOT"
INFRA_DIR=${INFRA_DIR:-"$ROOT/../infra"}
export COMPOSE_PROJECT_NAME=hprpr14-integration
export WORKER_CONCURRENCY=${WORKER_CONCURRENCY:-1}
compose=(docker compose --env-file /dev/null -f "$INFRA_DIR/compose.yaml" -f "$ROOT/test/integration/compose.yaml")
cd "$ROOT"
docker info >/dev/null
if [[ -n $(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME") ]]; then
  echo "A test project already exists; refusing to reuse or delete it" >&2
  exit 1
fi
python3 - <<'PY'
import socket
sockets = []
for port in (8080,8180,8333,9090):
    sock = socket.socket()
    sock.bind(('127.0.0.1',port))
    sockets.append(sock)
print('Ports 8080/8180/8333/9090 are available')
PY
cleanup() {
  result=$?
  trap - EXIT INT TERM
  if ! "${compose[@]}" down -v --remove-orphans --timeout 30; then
    echo "Unable to remove integration stack" >&2
    result=1
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${compose[@]}" build migrate api worker llmstub
"${compose[@]}" up -d postgres seaweedfs keycloak llmstub migrate api worker
export E2E_API_URL=http://localhost:8080
export E2E_KEYCLOAK_URL=http://localhost:8180
export E2E_DATABASE_URL="postgres://autocheck:autocheck@$("${compose[@]}" port postgres 5432)/autocheck?sslmode=disable"
# Не выводим env или конфигурацию Compose: там могут быть ключи магазина.
python3 - <<'PY'
import time, urllib.request
for attempt in range(180):
    try:
        for url in ('http://localhost:8080/readyz', 'http://localhost:8180/realms/autocheck/.well-known/openid-configuration'):
            with urllib.request.urlopen(url,timeout=2) as response:
                if response.status!=200: raise RuntimeError('not ready')
        break
    except Exception:
        if attempt==179: raise SystemExit('Integration services did not become ready')
        time.sleep(1)
print('Gateway and Keycloak are ready')
PY
if [[ ${1:-} == --load ]]; then
  mkdir -p test/load/results
  for entry in ${LOAD_MATRIX:-1:1 1:2 2:1 2:2 4:1}; do
    replicas=${entry%:*}
    export WORKER_CONCURRENCY=${entry#*:}
    "${compose[@]}" up -d --no-deps --scale "worker=$replicas" worker
    go run ./test/load -n "${LOAD_N:-40}" -concurrency "${LOAD_CONCURRENCY:-8}" \
      -workers "$replicas" -worker-concurrency "$WORKER_CONCURRENCY" \
      >"test/load/results/${replicas}x${WORKER_CONCURRENCY}.json"
    # JSON содержит только измерения, без токенов или presigned URL.
    cat "test/load/results/${replicas}x${WORKER_CONCURRENCY}.json"
  done
else
  go test -race -count=1 -tags=integration -timeout=30m -v ./test/integration
fi
