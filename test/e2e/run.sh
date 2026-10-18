#!/usr/bin/env bash
# Exercises the Kubernetes runtime contract from docs/adr/0008 on a throwaway
# multi-node kind cluster. It never touches your current kubectl context and
# never contacts upstream services.
#
# Usage: test/e2e/run.sh
# Environment:
#   CLUSTER  kind cluster name to create (default uatu-e2e); KEEP=1 keeps it.
#   CONTEXT  use this existing kubectl context instead of creating a kind
#            cluster. It needs at least two nodes. Set IMAGE to an image the
#            cluster can pull; the script does not build or push it.
set -euo pipefail

cd "$(dirname "$0")/../.."
CLUSTER="${CLUSTER:-uatu-e2e}"
EXTERNAL_CLUSTER=0
if [[ -n "${CONTEXT:-}" ]]; then
  EXTERNAL_CLUSTER=1
  [[ -n "${IMAGE:-}" ]] || { echo "IMAGE is required with CONTEXT" >&2; exit 2; }
else
  CONTEXT="kind-${CLUSTER}"
fi
E2E=test/e2e
IMAGE="${IMAGE:-uatu:e2e}"
POSTGRES_IMAGE=postgres:17
CURL_IMAGE=curlimages/curl:8.10.1
created_cluster=0
tmp="$(mktemp -d)"

k() { kubectl --context "$CONTEXT" "$@"; }
# manifest renders a manifest with the image under test.
manifest() { sed "s#__IMAGE__#$IMAGE#g" "$E2E/manifests/$1"; }
step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
db_url() { printf 'postgres://uatu:e2e-only@%s:5432/uatu?sslmode=disable' "$1"; }

cleanup() {
  local status=$?
  rm -rf "$tmp"
  if [[ $status -ne 0 ]]; then
    for ns in uatu-bundled uatu-external uatu-nofallback; do
      k -n "$ns" get pods -o wide 2>/dev/null || true
    done
  fi
  if [[ $created_cluster -eq 1 && "${KEEP:-0}" != 1 ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null
  fi
}
trap cleanup EXIT

setup_cluster() {
  if [[ $EXTERNAL_CLUSTER -eq 1 ]]; then
    step "Using existing context $CONTEXT with image $IMAGE"
    [[ "$(k get nodes --no-headers | wc -l | tr -d ' ')" -ge 2 ]] || fail "context needs at least two nodes"
    return
  fi
  step "Preparing kind cluster $CLUSTER"
  if ! kind get clusters | grep -qx "$CLUSTER"; then
    kind create cluster --name "$CLUSTER" --config "$E2E/kind-config.yaml" --wait 120s
    created_cluster=1
  fi
  docker build -q -t "$IMAGE" .
  docker image inspect "$POSTGRES_IMAGE" >/dev/null 2>&1 || docker pull -q "$POSTGRES_IMAGE"
  docker image inspect "$CURL_IMAGE" >/dev/null 2>&1 || docker pull -q "$CURL_IMAGE"
  for image in "$IMAGE" "$POSTGRES_IMAGE" "$CURL_IMAGE"; do
    kind load docker-image --name "$CLUSTER" "$image" >/dev/null
  done
}

new_namespace() {
  k delete namespace "$1" --ignore-not-found --wait=true >/dev/null
  k create namespace "$1" >/dev/null
  k -n "$1" create secret generic uatu-github --from-literal=token=e2e-not-a-real-token >/dev/null
  k -n "$1" create secret generic uatu-api-key --from-literal=key=e2e-api-key-0123456789-0123456789-abcdef >/dev/null
}

deploy_postgres() { # namespace name
  sed "s/__NAME__/$2/g" "$E2E/manifests/postgres.yaml" | k -n "$1" apply -f - >/dev/null
  k -n "$1" rollout status "statefulset/$2" --timeout=180s
}

set_db() { # namespace host
  k -n "$1" delete secret uatu-db --ignore-not-found >/dev/null
  k -n "$1" create secret generic uatu-db --from-literal=url="$(db_url "$2")" >/dev/null
}

run_migrations() { # namespace
  k -n "$1" delete job uatu-migrate --ignore-not-found --wait=true >/dev/null
  manifest migrate-job.yaml | k -n "$1" apply -f - >/dev/null
  k -n "$1" wait --for=condition=complete job/uatu-migrate --timeout=120s
}

psql_in() { # namespace statefulset sql
  k -n "$1" exec "$2-0" -- psql -U uatu -d uatu -Atc "$3"
}

wait_ready() { # namespace app replicas
  local _
  for _ in $(seq 1 90); do
    [[ "$(ready_count "$1" "$2")" -ge "$3" ]] && return 0
    sleep 2
  done
  fail "$2 has fewer than $3 ready pods"
}

ready_count() { # namespace app
  k -n "$1" get pods -l "app=$2" -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true
}

restart_count() { # namespace app
  k -n "$1" get pods -l "app=$2" -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | awk '{n += $1} END {print n + 0}'
}

# Runs a pod that polls the API Service until finish_probe stops it, and waits
# for a first successful request so later disruption is actually observed.
start_probe() { # namespace
  k -n "$1" delete pod probe --ignore-not-found --wait=true >/dev/null
  k -n "$1" run probe --image="$CURL_IMAGE" --image-pull-policy=IfNotPresent --restart=Never --command -- \
    sh -c 'while [ ! -f /tmp/stop ]; do curl -fsS -o /dev/null -m 2 http://uatu-api:3000/readyz && echo OK || echo FAIL; sleep 0.2; done' >/dev/null
  local _
  for _ in $(seq 1 60); do
    k -n "$1" logs probe 2>/dev/null | grep -q OK && return 0
    sleep 2
  done
  fail "probe never reached the API"
}

finish_probe() { # namespace
  k -n "$1" exec probe -- touch /tmp/stop
  k -n "$1" wait --for=jsonpath='{.status.phase}'=Succeeded pod/probe --timeout=60s >/dev/null
  local out
  out="$(k -n "$1" logs probe)"
  k -n "$1" delete pod probe --wait=false >/dev/null
  if grep -q FAIL <<<"$out"; then
    fail "API was unavailable during disruption ($(grep -c FAIL <<<"$out") failed requests)"
  fi
}

check_pod_hardening() { # namespace
  local ns="$1" app json
  for app in uatu-api uatu-worker; do
    json="$(k -n "$ns" get pods -l "app=$app" -o json)"
    python3 -c '
import json, sys
pods = json.load(sys.stdin)["items"]
assert pods, "no pods"
for pod in pods:
    c = pod["spec"]["containers"][0]
    sc = c["securityContext"]
    assert pod["spec"]["securityContext"]["runAsNonRoot"], "runAsNonRoot"
    assert sc["readOnlyRootFilesystem"] and not sc["allowPrivilegeEscalation"], "filesystem or escalation"
    assert sc["capabilities"]["drop"] == ["ALL"], "capabilities"
    assert c["resources"]["limits"]["memory"] and c["resources"]["limits"]["cpu"], "limits"
    for probe in ("startupProbe", "livenessProbe", "readinessProbe"):
        assert probe in c, probe
' <<<"$json" || fail "$app pods violate hardening requirements"
  done
}

scenario_bundled() {
  local ns=uatu-bundled
  step "[bundled] Provisioned PostgreSQL, serialized migrations, replicas across nodes"
  new_namespace "$ns"
  deploy_postgres "$ns" postgres
  set_db "$ns" postgres
  run_migrations "$ns"
  local versions files
  versions="$(psql_in "$ns" postgres "SELECT count(*) FROM schema_migrations")"
  files="$(find internal/store/migrations -name '*.sql' | wc -l | tr -d ' ')"
  [[ "$versions" == "$files" ]] || fail "two concurrent migrators recorded $versions migrations, want $files"
  manifest app.yaml | k -n "$ns" apply -f - >/dev/null
  wait_ready "$ns" uatu-api 2
  wait_ready "$ns" uatu-worker 2

  step "[bundled] Replicas run on distinct nodes with hardened security contexts"
  for app in uatu-api uatu-worker; do
    nodes="$(k -n "$ns" get pods -l "app=$app" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u | wc -l | tr -d ' ')"
    [[ "$nodes" -ge 2 ]] || fail "$app replicas run on $nodes node(s), want at least 2"
  done
  check_pod_hardening "$ns"

  step "[bundled] Connection use stays within replicas x UATU_DB_MAX_CONNS"
  local conns
  conns="$(psql_in "$ns" postgres "SELECT count(*) FROM pg_stat_activity WHERE usename = 'uatu' AND application_name <> 'psql' AND pid <> pg_backend_pid()")"
  [[ "$conns" -le 12 ]] || fail "application holds $conns connections, limit is 12 (4 pods x 3)"

  step "[bundled] Rolling upgrade and pod termination keep the API available"
  start_probe "$ns"
  k -n "$ns" rollout restart deployment/uatu-api deployment/uatu-worker >/dev/null
  k -n "$ns" rollout status deployment/uatu-api --timeout=180s
  k -n "$ns" rollout status deployment/uatu-worker --timeout=180s
  sleep 10 # let replaced pods finish terminating
  victim="$(k -n "$ns" get pods -l app=uatu-api --no-headers | awk '$2 == "1/1" && $3 == "Running" {print $1; exit}')"
  k -n "$ns" delete "pod/$victim" --ignore-not-found --wait=false >/dev/null
  wait_ready "$ns" uatu-api 2
  finish_probe "$ns"

  step "[bundled] Worker crash: killed pod is replaced and the other worker keeps serving"
  victim="$(k -n "$ns" get pods -l app=uatu-worker --no-headers | awk '$2 == "1/1" && $3 == "Running" {print $1; exit}')"
  k -n "$ns" delete "pod/$victim" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1
  wait_ready "$ns" uatu-worker 2

  step "[bundled] Database outage turns readiness off without restarting pods"
  local before after
  before=$(( $(restart_count "$ns" uatu-api) + $(restart_count "$ns" uatu-worker) ))
  k -n "$ns" scale statefulset/postgres --replicas=0 >/dev/null
  for _ in $(seq 1 60); do
    [[ "$(ready_count "$ns" uatu-api)" == 0 && "$(ready_count "$ns" uatu-worker)" == 0 ]] && break
    sleep 2
  done
  [[ "$(ready_count "$ns" uatu-api)" == 0 ]] || fail "API stayed ready during database outage"
  [[ "$(ready_count "$ns" uatu-worker)" == 0 ]] || fail "worker stayed ready during database outage"
  # Liveness fails after 3 x 10s, then termination takes up to 30s.
  sleep 80
  after=$(( $(restart_count "$ns" uatu-api) + $(restart_count "$ns" uatu-worker) ))
  [[ "$before" == "$after" ]] || fail "pods restarted during database outage ($before -> $after)"
  k -n "$ns" scale statefulset/postgres --replicas=1 >/dev/null
  k -n "$ns" rollout status statefulset/postgres --timeout=180s
  wait_ready "$ns" uatu-api 2
  wait_ready "$ns" uatu-worker 2

  step "[bundled] PostgreSQL backup and restore"
  psql_in "$ns" postgres "CREATE TABLE e2e_marker (note text); INSERT INTO e2e_marker VALUES ('kept')" >/dev/null
  k -n "$ns" exec postgres-0 -- pg_dump -U uatu -d uatu -Fc >"$tmp/backup.dump"
  psql_in "$ns" postgres "DROP SCHEMA public CASCADE; CREATE SCHEMA public" >/dev/null
  for _ in $(seq 1 30); do
    [[ "$(ready_count "$ns" uatu-api)" == 0 ]] && break
    sleep 2
  done
  [[ "$(ready_count "$ns" uatu-api)" == 0 ]] || fail "API stayed ready after the schema was dropped"
  k -n "$ns" exec -i postgres-0 -- pg_restore -U uatu -d uatu --no-owner <"$tmp/backup.dump"
  [[ "$(psql_in "$ns" postgres "SELECT note FROM e2e_marker")" == kept ]] || fail "restore lost data"
  wait_ready "$ns" uatu-api 2
}

scenario_external() {
  local ns=uatu-external
  step "[external] Workloads wait for migrations, then run against an external PostgreSQL"
  new_namespace "$ns"
  deploy_postgres "$ns" postgres-external
  set_db "$ns" postgres-external
  manifest app.yaml | k -n "$ns" apply -f - >/dev/null
  sleep 20
  [[ "$(ready_count "$ns" uatu-api)" == 0 ]] || fail "API became ready before migrations ran"
  run_migrations "$ns"
  wait_ready "$ns" uatu-api 2
  wait_ready "$ns" uatu-worker 2
}

scenario_nofallback() {
  local ns=uatu-nofallback
  step "[no fallback] Unreachable external database never falls back to the bundled one"
  new_namespace "$ns"
  deploy_postgres "$ns" postgres
  set_db "$ns" postgres-unreachable
  manifest migrate-job.yaml | k -n "$ns" apply -f - >/dev/null
  k -n "$ns" wait --for=condition=failed job/uatu-migrate --timeout=120s
  manifest app.yaml | k -n "$ns" apply -f - >/dev/null
  sleep 30
  [[ "$(ready_count "$ns" uatu-api)" == 0 ]] || fail "API became ready with an unreachable database"
  local tables
  tables="$(psql_in "$ns" postgres "SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'")"
  [[ "$tables" == 0 ]] || fail "bundled database was used despite the external failure"
}

setup_cluster
scenario_bundled
scenario_external
scenario_nofallback
step "All Kubernetes e2e checks passed"
