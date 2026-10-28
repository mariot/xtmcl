#!/usr/bin/env bash
# End-to-end test: drives a real XTM Composer with Docker through the CLI.
#
#   go build -o xtmcl . && ./scripts/e2e.sh
#   COMPOSER_IMAGE=xtm-composer:dev ./scripts/e2e.sh   # test a local Composer build
set -euo pipefail

BIN=${BIN:-./xtmcl}
COMPOSER_IMAGE=${COMPOSER_IMAGE:-filigran/xtm-composer:latest}
NAME="E2E Web"
CONTAINER=e2e-web
export XTMCL_HOME=${XTMCL_HOME:-$(mktemp -d)}

cleanup() {
  status=$?
  if [ $status -ne 0 ]; then
    echo "--- FAILED, Composer logs:"
    "$BIN" composer-logs 2>&1 | tail -30 || true
  fi
  "$BIN" down --purge >/dev/null 2>&1 || true
  exit $status
}
trap cleanup EXIT

# wait_for <description> <command...>: retry the command for up to 2 minutes
wait_for() {
  local description=$1
  shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then
      echo "ok: $description"
      return 0
    fi
    sleep 2
  done
  echo "timed out: $description" >&2
  return 1
}

container_state_is() { [ "$(docker inspect -f '{{.State.Status}}' "$CONTAINER" 2>/dev/null)" = "$1" ]; }
container_env_is() { [ "$(docker exec "$CONTAINER" printenv "$1" 2>/dev/null)" = "$2" ]; }
reported_status_is() { "$BIN" ps | grep -q "^$NAME .* $1 "; }
container_absent() { ! docker inspect "$CONTAINER" >/dev/null 2>&1; }

"$BIN" up --image "$COMPOSER_IMAGE"

echo "--- deploy, with a plain setting and a secret"
"$BIN" deploy nginx:alpine --name "$NAME" --env GREETING=hello --secret API_TOKEN=s3cr3t --timeout 3m
wait_for "the container runs" container_state_is running
wait_for "the setting is set" container_env_is GREETING hello
wait_for "the secret is decrypted by the Composer" container_env_is API_TOKEN s3cr3t
docker exec "$CONTAINER" printenv OPENAEV_URL | grep -q '^http://' && echo "ok: OPENAEV_URL points at the platform"
wait_for "the Composer reports the logs" sh -c "'$BIN' logs '$NAME' | grep -q nginx"

echo "--- stop and start"
"$BIN" stop "$NAME"
wait_for "the container stops" container_state_is exited
wait_for "the Composer reports it stopped" reported_status_is stopped
"$BIN" start "$NAME"
wait_for "the container runs again" container_state_is running
wait_for "the Composer reports it started" reported_status_is started

echo "--- update a setting: the Composer redeploys"
"$BIN" deploy nginx:alpine --name "$NAME" --env GREETING=bonjour --secret API_TOKEN=s3cr3t --wait=false
wait_for "the new setting is deployed" container_env_is GREETING bonjour

echo "--- remove"
"$BIN" rm "$NAME"
wait_for "the container is removed" container_absent

"$BIN" down
if docker ps -a --format '{{.Names}}' | grep -q '^xtmcl-'; then
  echo "the Composer container is still there" >&2
  exit 1
fi
echo "PASS"
