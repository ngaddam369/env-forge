#!/usr/bin/env bash
# demo.sh — Interactive demonstration of env-forge, saga-conductor, and svid-exchange
#
# ── What this script does ─────────────────────────────────────────────────────
#
#   env-forge provisions isolated AWS developer environments (VPC, RDS, EC2, S3)
#   using two independently-developed infrastructure libraries as its engine:
#
#     saga-conductor  — distributed transaction orchestration (push-model sagas)
#     svid-exchange   — zero-trust token exchange (SPIFFE SVID → ES256 JWT)
#
#   This script walks through 14 demo moments that exercise every feature surface
#   of both libraries. The demos are grouped into two modes:
#
#   LOCAL MODE (no Kubernetes):
#     Starts saga-conductor, forge-api, and forge-worker as local binaries.
#     All AWS calls are stubbed (--dry-run). JWT auth is disabled (dev mode).
#     Covers all saga-conductor features: crash recovery, backward compensation,
#     dead-lettering, saga listing, abort, metrics, graceful drain, idempotency.
#
#   MINIKUBE MODE (requires SPIRE + minikube):
#     Services run as Kubernetes deployments with SPIRE for workload identity.
#     forge-worker obtains a SPIFFE SVID, exchanges it for an ES256 JWT via
#     svid-exchange, and attaches the JWT to every forge-api call.
#     Covers all svid-exchange features: policy CRUD, token exchange, policy
#     reload, token revocation, scope enforcement, key rotation, rate limiting.
#
# ── Why env-forge as the demo vehicle? ───────────────────────────────────────
#
#   Provisioning a developer environment is a naturally multi-step operation
#   where each step has side effects (AWS resources, DNS entries, SPIRE policies).
#   This makes it the ideal vehicle for demonstrating saga orchestration:
#   - If any step fails, all prior steps must be undone (compensation cascade).
#   - Steps can crash halfway through (idempotency / crash recovery).
#   - The process must be observable, pausable, and debuggable (metrics, SSE).
#   Once the environment is provisioned, it requires zero-trust auth between its
#   services, demonstrating the full svid-exchange lifecycle naturally.
#
# ── Usage ─────────────────────────────────────────────────────────────────────
#
#   ./demo.sh [--local | --minikube | --all] [--auto] [--demo N]
#
#   --local     Demos 1,2,5,6,7,8,9,10  — saga-conductor features, local binaries (default)
#   --minikube  Demos 3,4,11,12,13,14   — svid-exchange + JWT, requires minikube + SPIRE
#   --all       All 14 demos             — local first, then minikube
#   --auto      Non-interactive          — no pause prompts (CI-friendly)
#   --demo N    Single demo N (1-14)     — starts services for the relevant mode first

set -euo pipefail

# ─── Paths ────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONDUCTOR_DIR="$(cd "$SCRIPT_DIR/../saga-conductor" 2>/dev/null && pwd || echo "$SCRIPT_DIR/../saga-conductor")"

FORGE_BIN="$SCRIPT_DIR/bin/forge"
FORGE_API_BIN="$SCRIPT_DIR/bin/forge-api"
FORGE_WORKER_BIN="$SCRIPT_DIR/bin/forge-worker"
CONDUCTOR_BIN="$CONDUCTOR_DIR/bin/saga-conductor"

CONDUCTOR_DB="/tmp/demo-saga.db"
FORGE_DB="/tmp/demo-forge.db"
LOG_DIR="/tmp/demo-logs"
CONDUCTOR_LOG="$LOG_DIR/conductor.log"
FORGE_API_LOG="$LOG_DIR/forge-api.log"
FORGE_WORKER_LOG="$LOG_DIR/forge-worker.log"

# ─── Colors ───────────────────────────────────────────────────────────────────

if [[ -t 1 ]]; then
  BOLD='\033[1m'; RESET='\033[0m'
  RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
  CYAN='\033[0;36m'; DIM='\033[2m'
else
  BOLD=''; RESET=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; DIM=''
fi

# ─── State ────────────────────────────────────────────────────────────────────

CONDUCTOR_PID=0
FORGE_API_PID=0
FORGE_WORKER_PID=0

PF_CONDUCTOR_PID=0
PF_FORGE_API_PID=0
PF_FORGE_WORKER_PID=0
PF_SVID_ADMIN_PID=0
PF_SVID_HEALTH_PID=0

RUN_MODE="local"
AUTO_MODE="false"
DEMO_ONLY=""

# ─── Cleanup ──────────────────────────────────────────────────────────────────

kill_if_running() {
  local pid="${1:-0}"
  [[ "$pid" -le 0 ]] && return 0
  if kill -0 "$pid" 2>/dev/null; then
    kill -SIGTERM "$pid" 2>/dev/null || true
    local i
    for i in 1 2 3 4 5 6; do
      sleep 0.5
      kill -0 "$pid" 2>/dev/null || return 0
    done
    kill -SIGKILL "$pid" 2>/dev/null || true
  fi
}

cleanup() {
  echo ""
  kill_if_running "$FORGE_WORKER_PID"
  kill_if_running "$FORGE_API_PID"
  kill_if_running "$CONDUCTOR_PID"
  kill_if_running "$PF_CONDUCTOR_PID"
  kill_if_running "$PF_FORGE_API_PID"
  kill_if_running "$PF_FORGE_WORKER_PID"
  kill_if_running "$PF_SVID_ADMIN_PID"
  kill_if_running "$PF_SVID_HEALTH_PID"
  # Kill any kubectl port-forward children that outlived their parent restart-loop subshell.
  pkill -f "kubectl port-forward" 2>/dev/null || true
  rm -f "$CONDUCTOR_DB" "$FORGE_DB"
}

trap cleanup EXIT

# ─── UX Helpers ───────────────────────────────────────────────────────────────

header() {
  local title="$*"
  local len=${#title}
  local line
  line=$(printf '═%.0s' $(seq 1 $((len + 4))))
  echo ""
  echo -e "${BOLD}${CYAN}${line}${RESET}"
  echo -e "${BOLD}${CYAN}  ${title}  ${RESET}"
  echo -e "${BOLD}${CYAN}${line}${RESET}"
  echo ""
}

subheader() { echo -e "\n${BOLD}${YELLOW}▶ $*${RESET}"; }
info()      { echo -e "${DIM}  ℹ  $*${RESET}"; }
success()   { echo -e "${GREEN}  ✓  $*${RESET}"; }
warn()      { echo -e "${YELLOW}  ⚠  $*${RESET}"; }

fail() {
  echo -e "${RED}  ✗  $*${RESET}" >&2
  exit 1
}

pause() {
  if [[ "$AUTO_MODE" == "false" ]]; then
    echo ""
    echo -e "${DIM}  ── Press ENTER to continue ──${RESET}"
    read -r
  else
    sleep 1
  fi
}

# describe — prints a multi-line explanation block before a demo step.
# Use for "why does this feature exist?" context that goes beyond a one-liner.
# Each argument is one line of the description.
describe() {
  echo ""
  for line in "$@"; do
    echo -e "${DIM}     ${line}${RESET}"
  done
  echo ""
}

# diagram — prints an ASCII/Unicode state or flow diagram inside a dim box.
# Each argument is one line of the diagram body.
diagram() {
  echo ""
  echo -e "${DIM}     ┌──────────────────────────────────────────────────────────────┐${RESET}"
  for line in "$@"; do
    echo -e "${DIM}     │  ${line}${RESET}"
  done
  echo -e "${DIM}     └──────────────────────────────────────────────────────────────┘${RESET}"
  echo ""
}

# show_overview — prints the general service topology and saga state machine.
# Called once at the start of each phase so the viewer has the full picture.
show_overview() {
  subheader "System overview"
  diagram \
    "SERVICE TOPOLOGY" \
    "" \
    "  forge CLI" \
    "      │  POST /envs/create" \
    "      ▼" \
    "  forge-api (:9090)  ──gRPC CreateSaga+StartSaga──►  saga-conductor (:8080)" \
    "      │  BoltDB (env state)                               │  POST /steps/{name}" \
    "      │                                                   ▼" \
    "      │                                           forge-worker (:9091)" \
    "      │◄──GET|PUT /internal/envs/{id}─────────────────────┤" \
    "      │   Authorization: Bearer JWT (minikube only)        ├── AWS (VPC/RDS/EC2/S3)" \
    "                                                           └── svid-exchange (policies)" \
    "" \
    "SAGA STATE MACHINE" \
    "" \
    "  PENDING ──► RUNNING ──────────────────────────────────► COMPLETED" \
    "                 │  step fails                                        " \
    "                 ▼                                                    " \
    "            COMPENSATING ──────────────────────────────► FAILED      " \
    "                 │  compensation retries exhausted                    " \
    "                 ▼                                                    " \
    "          COMPENSATION_FAILED  (dead-letter — manual intervention)   " \
    "                                                                      " \
    "  RUNNING ──► AbortSaga() ──────────────────────────────► ABORTED    " \
    "" \
    "8 SAGA STEPS (in order, each has Execute + Compensate + IsAlreadyDone)" \
    "  1:vpc  2:rds  3:ec2  4:s3  5:identity  6:config  7:health  8:registry"
}

# Print and execute a command (no pipes — use run_pipe for those)
run_cmd() {
  echo -e "${CYAN}  \$${RESET} $*"
  "$@"
}

# Print and execute a shell pipeline or complex expression
run_pipe() {
  local display="$1"; shift
  echo -e "${CYAN}  \$${RESET} $display"
  eval "$@"
}

# ─── Prerequisite Checks ──────────────────────────────────────────────────────

check_port_free() {
  local port="$1"
  if ss -tlnp 2>/dev/null | grep -q ":${port} " || \
     netstat -tlnp 2>/dev/null | grep -q ":${port} "; then
    fail "Port $port is already in use. Stop the conflicting process and retry."
  fi
}

check_prereqs_local() {
  info "Checking local prerequisites..."

  command -v mise >/dev/null 2>&1 || fail "mise not found. Install from https://mise.jdx.dev/"

  [[ -d "$CONDUCTOR_DIR" ]] || \
    fail "saga-conductor not found at $CONDUCTOR_DIR. Expected: ~/go_projects/saga-conductor"

  for port in 8080 8081 9090 9091; do
    check_port_free "$port"
  done

  success "Prerequisites OK"
}

check_prereqs_minikube() {
  info "Checking minikube prerequisites..."

  command -v minikube >/dev/null 2>&1 || fail "minikube not found."
  command -v kubectl   >/dev/null 2>&1 || fail "kubectl not found."

  minikube status 2>/dev/null | grep -q "Running" || \
    fail "minikube is not running. Run: minikube start"

  for svc in saga-conductor svid-exchange forge-api forge-worker; do
    kubectl get deploy "$svc" -n default >/dev/null 2>&1 || \
      fail "Deployment '$svc' not found. Follow docs/minikube-setup.md first."
  done

  [[ -x "$FORGE_BIN" ]] || fail "forge binary not found. Run: make build"

  success "Minikube prerequisites OK"
}

# ─── Build Helpers ────────────────────────────────────────────────────────────

build_if_needed() {
  local bin="$1" src_dir="$2" name="$3"
  if [[ ! -x "$bin" ]] || find "$src_dir" -name '*.go' -newer "$bin" -print -quit 2>/dev/null | grep -q .; then
    info "Building $name..."
    local build_log
    build_log=$(mktemp)
    if ! (cd "$src_dir" && make build) > "$build_log" 2>&1; then
      warn "Build failed. Last 20 lines:"
      tail -20 "$build_log" >&2
      rm -f "$build_log"
      fail "Build of $name failed."
    fi
    rm -f "$build_log"
    success "Built $name"
  else
    info "$name binary is up to date"
  fi
}

# ─── Service Lifecycle ────────────────────────────────────────────────────────

# start_conductor [EXTRA_KEY=VAL...]
# Extra KEY=VAL args override defaults (e.g. "SHUTDOWN_SAGA_TIMEOUT_SECONDS=2").
#
# SHUTDOWN_DRAIN_SECONDS=0: Demo 9 (graceful drain) measures drain time directly
#   by waiting for the saga to complete naturally. Setting this to 0 means conductor
#   won't add extra idle wait time — it exits as soon as Drain() finishes.
# SHUTDOWN_SAGA_TIMEOUT_SECONDS=60: Allows up to 60s for an in-flight saga to
#   complete during drain. The full dry-run takes ~21s so this is more than enough.
start_conductor() {
  (
    export DB_PATH="$CONDUCTOR_DB"
    export HEALTH_ADDR=":8081"
    export GRPC_ADDR=":8080"
    export SHUTDOWN_DRAIN_SECONDS="0"
    export SHUTDOWN_SAGA_TIMEOUT_SECONDS="60"
    export GRPC_STOP_TIMEOUT_SECONDS="30"
    for kv in "$@"; do export "$kv"; done
    exec "$CONDUCTOR_BIN"
  ) >> "$CONDUCTOR_LOG" 2>&1 &
  CONDUCTOR_PID=$!
}

# start_forge_api [EXTRA_KEY=VAL...]
#
# SVIDEXCHANGE_JWKS_URL omitted: In local mode there's no SPIRE or svid-exchange.
#   forge-api drops into "dev mode" — all /internal/* requests are accepted without
#   a JWT. This is safe for local demos because forge-worker is also local and trusted.
#   In minikube mode (Demo 4+), SVIDEXCHANGE_JWKS_URL is set in the K8s ConfigMap
#   and JWT validation is enforced.
start_forge_api() {
  (
    export DB_PATH="$FORGE_DB"
    export STEP_ADDR=":9090"
    export CONDUCTOR_ADDR="localhost:8080"
    export FORGE_WORKER_URL="http://localhost:9091"
    # SVIDEXCHANGE_JWKS_URL intentionally omitted → dev mode (JWT validation disabled)
    for kv in "$@"; do export "$kv"; done
    exec "$FORGE_API_BIN"
  ) >> "$FORGE_API_LOG" 2>&1 &
  FORGE_API_PID=$!
}

# start_forge_worker [EXTRA_KEY=VAL...]
#
# SPIFFE_ENDPOINT_SOCKET omitted: Without a SPIRE socket, forge-worker cannot obtain
#   an SVID and therefore cannot exchange it for a JWT. It calls forge-api without
#   an Authorization header. forge-api's dev mode accepts this. In minikube mode,
#   the SPIRE agent DaemonSet mounts the socket at /run/spire/sockets/agent.sock and
#   forge-worker uses it automatically.
start_forge_worker() {
  (
    export FORGE_API_URL="http://localhost:9090"
    export WORKER_ADDR=":9091"
    # SPIFFE_ENDPOINT_SOCKET intentionally omitted → dev mode (no JWT exchange)
    for kv in "$@"; do export "$kv"; done
    exec "$FORGE_WORKER_BIN"
  ) >> "$FORGE_WORKER_LOG" 2>&1 &
  FORGE_WORKER_PID=$!
}

wait_healthy() {
  local url="$1" timeout="${2:-30}"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    if curl -sf --connect-timeout 1 "$url" >/dev/null 2>&1; then
      return 0
    fi
    printf "  [%ds] waiting for %s...\r" "$elapsed" "$url"
    sleep 0.5
    elapsed=$((elapsed + 1))
  done
  echo ""
  fail "Timed out (${timeout}s) waiting for $url"
}

kill_worker_only() {
  kill_if_running "$FORGE_WORKER_PID"
  FORGE_WORKER_PID=0
}

restart_worker() {
  kill_worker_only
  start_forge_worker "$@"
  wait_healthy "http://localhost:9091/health/live" 15
}

start_all_local() {
  mkdir -p "$LOG_DIR"
  : > "$CONDUCTOR_LOG"
  : > "$FORGE_API_LOG"
  : > "$FORGE_WORKER_LOG"

  info "Starting saga-conductor..."
  start_conductor
  wait_healthy "http://localhost:8081/health/live" 30

  info "Starting forge-api..."
  start_forge_api
  wait_healthy "http://localhost:9090/health/live" 30

  info "Starting forge-worker..."
  start_forge_worker
  wait_healthy "http://localhost:9091/health/live" 30

  success "All services running (conductor PID=$CONDUCTOR_PID, api PID=$FORGE_API_PID, worker PID=$FORGE_WORKER_PID)"
}

reset_services_between_demos() {
  info "Resetting services for next demo..."
  kill_if_running "$FORGE_WORKER_PID"; FORGE_WORKER_PID=0
  kill_if_running "$FORGE_API_PID";    FORGE_API_PID=0
  kill_if_running "$CONDUCTOR_PID";    CONDUCTOR_PID=0
  rm -f "$CONDUCTOR_DB" "$FORGE_DB"
  : > "$CONDUCTOR_LOG"
  : > "$FORGE_API_LOG"
  : > "$FORGE_WORKER_LOG"
  start_conductor
  wait_healthy "http://localhost:8081/health/live" 30
  start_forge_api
  wait_healthy "http://localhost:9090/health/live" 30
  start_forge_worker
  wait_healthy "http://localhost:9091/health/live" 30
}

# ─── Minikube Port-Forwards ───────────────────────────────────────────────────

start_port_forward() {
  local svc="$1" local_port="$2" remote_port="$3" pid_var="$4"
  # Run in a restart loop — kubectl port-forward drops after ~minutes of inactivity
  # or when a pod is replaced. The loop reconnects automatically.
  (while true; do
    kubectl port-forward "svc/$svc" "${local_port}:${remote_port}" \
      --namespace default >/dev/null 2>&1
    sleep 1
  done) &
  local pid=$!
  eval "${pid_var}=${pid}"
}

setup_minikube_port_forwards() {
  info "Setting up port-forwards..."

  start_port_forward saga-conductor 8081 8081 PF_CONDUCTOR_PID
  sleep 1
  wait_healthy "http://localhost:8081/health/live" 30

  start_port_forward forge-api      9090 9090 PF_FORGE_API_PID
  sleep 1
  wait_healthy "http://localhost:9090/health/live" 30

  start_port_forward forge-worker   9091 9091 PF_FORGE_WORKER_PID
  sleep 1
  wait_healthy "http://localhost:9091/health/live" 30

  # svid-exchange port-forward mapping:
  #   8082→8082: admin gRPC (PolicyAdmin service) — used by forge CLI policy/token commands
  #   8083→8081: health/JWKS HTTP endpoint — mapped to 8083 locally because saga-conductor
  #              also uses 8081 for its health/metrics endpoint and both are port-forwarded.
  #              Demo 14 hits http://localhost:8083/jwks to show the JWKS key set.
  start_port_forward svid-exchange  8082 8082 PF_SVID_ADMIN_PID
  sleep 1
  start_port_forward svid-exchange  8083 8081 PF_SVID_HEALTH_PID
  sleep 1

  success "Port-forwards ready"
}

# ─── State Polling Helpers ────────────────────────────────────────────────────

# get_env_prefix_for_owner OWNER [TIMEOUT_SECS]
# Polls forge list until an env row with that owner appears; echoes the 8-char prefix.
get_env_prefix_for_owner() {
  local owner="$1" timeout="${2:-30}"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    local prefix list_out
    list_out=$("$FORGE_BIN" list 2>/dev/null) || true
    prefix=$(echo "$list_out" | awk -v o="$owner" 'NR>1 && $2==o {print $1; exit}')
    if [[ -n "$prefix" ]]; then
      echo "$prefix"
      return 0
    fi
    sleep 0.5
    elapsed=$((elapsed + 1))
  done
  fail "Timed out (${timeout}s) waiting for environment with owner=$owner"
}

# wait_for_saga_state OWNER ENV_STATUS [TIMEOUT_SECS]
# Polls forge list until the env row status matches ENV_STATUS.
wait_for_saga_state() {
  local owner="$1" expected="$2" timeout="${3:-90}"
  local elapsed=0
  local status=""
  while [[ $elapsed -lt $timeout ]]; do
    local list_out
    list_out=$("$FORGE_BIN" list 2>/dev/null) || true
    status=$(echo "$list_out" | awk -v o="$owner" 'NR>1 && $2==o {print $3; exit}')
    if [[ "$status" == "$expected" ]]; then
      echo ""
      return 0
    fi
    printf "  [%ds] env status=%s  (waiting for %s)...\r" "$elapsed" "${status:-unknown}" "$expected"
    sleep 2
    elapsed=$((elapsed + 2))
  done
  echo ""
  fail "Timed out (${timeout}s) waiting for owner=$owner env status=$expected (last: ${status:-unknown})"
}

# wait_for_saga_conductor_status STATUS_SUBSTR [TIMEOUT_SECS]
# Polls forge sagas list until any row contains STATUS_SUBSTR (e.g. COMPENSATING).
wait_for_saga_conductor_status() {
  local substr="$1" timeout="${2:-120}"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    local sagas_out
    sagas_out=$("$FORGE_BIN" sagas list 2>/dev/null) || true
    if echo "$sagas_out" | grep -q "$substr"; then
      echo ""
      return 0
    fi
    printf "  [%ds] waiting for saga status containing '%s'...\r" "$elapsed" "$substr"
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo ""
  fail "Timed out (${timeout}s) waiting for saga conductor status: $substr"
}

# ─── Demo Functions ───────────────────────────────────────────────────────────

demo_01_crash_recovery() {
  header "Demo 1 — Crash Recovery (saga-conductor)"
  describe \
    "PROBLEM: In a distributed system, any component can crash at any moment." \
    "If saga-conductor simply replays a failed step from scratch, it might duplicate" \
    "an AWS resource — two VPCs, two RDS instances — leaving orphaned cloud spend." \
    "" \
    "SOLUTION: saga-conductor guarantees at-least-once step delivery (it will keep" \
    "retrying until a step succeeds), while each step implements IsAlreadyDone() to" \
    "turn that into exactly-once effect. IsAlreadyDone() checks whether the resource" \
    "was already created (e.g. 'VPCID != empty string in BoltDB') and returns true" \
    "if so, causing the step to no-op harmlessly." \
    "" \
    "HOW: We kill forge-worker 4 seconds into a provisioning run. At that point:" \
    "  - vpc step (2s) has already written its VPCID to BoltDB → IsAlreadyDone=true" \
    "  - rds step (8s) is mid-sleep, has NOT yet written RDSInstanceID → IsAlreadyDone=false" \
    "After forge-worker restarts, conductor retries rds. The vpc step is safely skipped."
  info "Watch for: 'skipping step vpc (already done)' vs rds re-executing."
  diagram \
    "CRASH RECOVERY FLOW" \
    "" \
    "  t=0s   vpc  ─── Execute() ─── sleep 2s ─── write VPCID ──► DONE ✓" \
    "  t=2s   rds  ─── Execute() ─── sleep 5s ───────────────────────────►" \
    "  t=4s   [forge-worker KILLED]  ◄── conductor retries rds ──────────►" \
    "                                                                       " \
    "  Retry (forge-worker restarted):                                      " \
    "         vpc  ─── IsAlreadyDone? ──► YES (VPCID in BoltDB) ──► SKIP  " \
    "         rds  ─── IsAlreadyDone? ──► NO  (no RDSInstanceID) ──► RUN  " \
    "         ... ec2 → s3 → identity → config → health → registry ...     " \
    "                                                                       " \
    "  Final: saga COMPLETED, all 8 steps SUCCEEDED (vpc counted once)    "
  pause

  subheader "Starting dry-run provisioning in the background..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo1 --dry-run &"
  "$FORGE_BIN" create --owner demo1 --dry-run &

  info "Waiting for vpc step to complete (~2s) and rds step to start sleeping (~5s)..."
  sleep 4

  local prefix
  prefix=$(get_env_prefix_for_owner demo1 10)
  info "Environment ID prefix: $prefix"

  subheader "Killing forge-worker (simulating a crash mid-rds-step)..."
  info "forge-worker PID=$FORGE_WORKER_PID — sending SIGKILL"
  kill -SIGKILL "$FORGE_WORKER_PID" 2>/dev/null || true
  FORGE_WORKER_PID=0
  success "forge-worker is dead. saga-conductor will now retry the rds step."
  pause

  subheader "Restarting forge-worker..."
  start_forge_worker
  wait_healthy "http://localhost:9091/health/live" 15
  success "forge-worker restarted (PID=$FORGE_WORKER_PID)"
  info "Conductor retries rds step. forge-worker calls IsAlreadyDone for each step."
  info "vpc: IsAlreadyDone=true (VPCID already in BoltDB) — SKIPPED."
  info "rds: IsAlreadyDone=false (RDSInstanceID not yet written) — RE-EXECUTED."
  pause

  subheader "Waiting for saga to complete..."
  wait_for_saga_state demo1 ready 120

  run_cmd "$FORGE_BIN" list
  run_cmd "$FORGE_BIN" status "$prefix"

  success "Demo 1 complete: saga recovered without duplicating vpc. Crash recovery works."
  pause
}

demo_02_compensation_cascade() {
  header "Demo 2 — Compensation Cascade (saga-conductor)"
  describe \
    "PROBLEM: Provisioning touches multiple independent systems (AWS, SPIRE, databases)." \
    "You cannot wrap all of them in a single ACID transaction — they don't share a" \
    "transaction coordinator. If step 7 (health check) fails after steps 1–6 have" \
    "already created a VPC, RDS instance, EC2 node, S3 bucket, and SPIRE policies," \
    "you need a way to undo all of that work cleanly." \
    "" \
    "SOLUTION: The saga pattern. Each step registers both an execute URL and a" \
    "compensate URL with saga-conductor. When any step fails, conductor calls the" \
    "compensate endpoint for each previously-completed step in reverse order." \
    "Compensation is itself retried with backoff — it's also at-least-once." \
    "" \
    "HOW: --fail-at-health injects an error at step 7 (the SELECT 1 health check)." \
    "Conductor transitions: RUNNING → COMPENSATING → FAILED." \
    "Steps are compensated in exact reverse: config→identity→s3→ec2→rds→vpc." \
    "(registry was never reached; health has a no-op compensate.)"
  info "Watch for: saga status=FAILED, all 6 prior steps showing STEP_STATUS_COMPENSATED."
  diagram \
    "COMPENSATION CASCADE FLOW" \
    "" \
    "  Forward (RUNNING):                                                   " \
    "    1:vpc ──► 2:rds ──► 3:ec2 ──► 4:s3 ──► 5:identity ──► 6:config   " \
    "      ✓         ✓         ✓         ✓           ✓              ✓       " \
    "                                                        7:health ──► FAIL ✗" \
    "                                                                │       " \
    "                                         saga: COMPENSATING ◄──┘       " \
    "                                                                        " \
    "  Backward (COMPENSATING — strict reverse order):                      " \
    "    6:config ──► 5:identity ──► 4:s3 ──► 3:ec2 ──► 2:rds ──► 1:vpc   " \
    "      comp            comp       comp      comp      comp       comp    " \
    "                                                                        " \
    "  Final state: saga FAILED                                             " \
    "  (8:registry was never reached; 7:health compensate is a no-op)      "
  pause

  subheader "Provisioning with --fail-at-health (blocking until terminal state)..."
  run_cmd "$FORGE_BIN" create --owner demo2 --dry-run --fail-at-health

  local prefix
  prefix=$(get_env_prefix_for_owner demo2 10)

  subheader "Inspecting the result:"
  run_cmd "$FORGE_BIN" list
  run_cmd "$FORGE_BIN" status "$prefix"

  info "saga status=SAGA_STATUS_FAILED, all prior steps show STEP_STATUS_COMPENSATED."
  success "Demo 2 complete: full backward compensation cascade executed automatically."
  pause
}

demo_05_dead_lettering() {
  header "Demo 5 — Dead-lettering (saga-conductor)"
  describe \
    "PROBLEM: Compensation can fail too. What happens if forge-worker crashes *during*" \
    "compensation? Infinite retries would spin forever and block other work. You need" \
    "a terminal bad state that surfaces the incident to an operator." \
    "" \
    "SOLUTION: saga-conductor counts compensation retries per step. When max_retries" \
    "is exhausted on any compensation step, the saga moves to COMPENSATION_FAILED —" \
    "a permanent terminal state ('dead-letter'). The saga is preserved in BoltDB" \
    "exactly as-is across conductor restarts, waiting for manual remediation." \
    "" \
    "HOW: We kill forge-worker at t=11s, after vpc(2s)+rds(8s) complete but before" \
    "ec2 starts. Conductor tries to execute ec2 — connection refused — retries with" \
    "backoff — exhausts retries — transitions to COMPENSATING. Compensation calls for" \
    "rds also get connection refused. After max_retries: COMPENSATION_FAILED." \
    "" \
    "WHY THIS MATTERS: Cloud resources (VPC, RDS) now exist with no cleanup. The" \
    "COMPENSATION_FAILED state is a page-worthy incident. The operator must manually" \
    "inspect what was created and clean up. It's a deliberate design choice to surface" \
    "this rather than hide it."
  info "Watch for: saga status transitioning RUNNING → COMPENSATING → COMPENSATION_FAILED."
  diagram \
    "DEAD-LETTER FLOW" \
    "" \
    "  t=0s   1:vpc  ──► DONE ✓  (2s)" \
    "  t=2s   2:rds  ──► DONE ✓  (8s)" \
    "  t=10s  [forge-worker KILLED]" \
    "  t=10s  3:ec2  ──► POST /steps/ec2  ──► connection refused" \
    "                ──► retry (backoff) ──► connection refused" \
    "                ──► retry (backoff) ──► connection refused" \
    "                ──► max_retries exhausted ──► saga: COMPENSATING" \
    "" \
    "  COMPENSATING:                                                          " \
    "         2:rds.compensate ──► connection refused ──► retry ──► retry    " \
    "                          ──► max_retries exhausted                      " \
    "                                                │                        " \
    "                                                ▼                        " \
    "                                   saga: COMPENSATION_FAILED  ← dead-letter" \
    "" \
    "  Persists in BoltDB across conductor restarts. Manual cleanup required."
  pause

  subheader "Starting dry-run provisioning..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo5 --dry-run &"
  "$FORGE_BIN" create --owner demo5 --dry-run &

  info "Waiting 11s for vpc(2s) + rds(8s) to complete, then killing forge-worker..."
  sleep 11

  subheader "Killing forge-worker (ec2 compensation will now get connection refused)..."
  kill -SIGKILL "$FORGE_WORKER_PID" 2>/dev/null || true
  FORGE_WORKER_PID=0
  success "forge-worker dead. Conductor will retry ec2 forward (fails), then rds compensation (fails)."
  info "Expected timeline: ec2 forward retries (~14s) + rds compensation retries (~14s) = ~28s"
  pause

  subheader "Waiting for COMPENSATION_FAILED state (may take ~30s)..."
  wait_for_saga_conductor_status "COMPENSATION_FAILED" 120

  run_cmd "$FORGE_BIN" sagas list
  info "Saga is now in COMPENSATION_FAILED (dead-letter). Manual intervention required."
  info "This state persists in BoltDB across conductor restarts."

  subheader "Restarting forge-worker for subsequent demos..."
  start_forge_worker
  wait_healthy "http://localhost:9091/health/live" 15
  success "forge-worker restored."

  success "Demo 5 complete: COMPENSATION_FAILED (dead-letter) state observed."
  pause
}

demo_06_list_sagas_step_detail() {
  header "Demo 6 — ListSagas + Step Detail (saga-conductor)"
  describe \
    "PROBLEM: When you have many concurrent sagas (multiple teams provisioning envs)," \
    "you need to be able to answer: 'which sagas are currently running?', 'why did" \
    "this one fail?', 'what step is it stuck on?'" \
    "" \
    "SOLUTION: saga-conductor's ListSagas RPC supports:" \
    "  - Status filter: 'show me only RUNNING sagas' / 'show me all FAILED sagas'" \
    "  - Cursor-based pagination: stable result sets even as sagas are added/completed" \
    "  - GetSaga: full per-step breakdown with start time, end time, and error_detail" \
    "" \
    "The 'forge status' command wraps GetSaga and formats the step execution table." \
    "The error_detail field is a JSON string set by the step's Execute() error return —" \
    "it can contain structured context (AWS error codes, HTTP status, etc.)." \
    "" \
    "WHY PAGINATION MATTERS: A production conductor can have thousands of completed" \
    "sagas. Returning them all in a single RPC call would OOM the client. Cursor-based" \
    "pagination uses a stable BoltDB cursor — consistent regardless of concurrent writes."
  info "Watch for: --status=RUNNING filter, step timestamps, pagination cursor token."
  diagram \
    "ListSagas + GetSaga + PAGINATION" \
    "" \
    "  Two concurrent sagas:" \
    "    saga-A (demo6a): RUNNING  [vpc✓  rds✓  ec2→  ...         ]" \
    "    saga-B (demo6b): RUNNING  [vpc✓  rds→              ...   ]" \
    "" \
    "  ListSagas(status=RUNNING)  ──► [saga-A, saga-B]             " \
    "  GetSaga(saga-A)            ──► step breakdown:               " \
    "    vpc      SUCCEEDED  started=T+0.1s  completed=T+2.1s       " \
    "    rds      SUCCEEDED  started=T+2.1s  completed=T+10.3s      " \
    "    ec2      RUNNING    started=T+10.3s                         " \
    "" \
    "  Cursor-based pagination (stable across concurrent writes):   " \
    "    ListSagas(page_size=1)           ──► [saga-A]  cursor=<tok>" \
    "    ListSagas(page_size=1, cursor=<tok>) ──► [saga-B]  cursor=''"
  pause

  subheader "Starting two concurrent dry-run sagas..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo6a --dry-run &"
  "$FORGE_BIN" create --owner demo6a --dry-run &
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo6b --dry-run &"
  "$FORGE_BIN" create --owner demo6b --dry-run &

  info "Sleeping 3s so both sagas are in the rds step (RUNNING)..."
  sleep 3

  subheader "List all RUNNING sagas (saga-conductor ListSagas with status filter):"
  run_cmd "$FORGE_BIN" sagas list --status=RUNNING

  subheader "List all environments:"
  run_cmd "$FORGE_BIN" list

  subheader "Step-level detail for demo6a (GetSaga with StepExecution breakdown):"
  local prefix
  prefix=$(get_env_prefix_for_owner demo6a 10)
  run_cmd "$FORGE_BIN" status "$prefix"

  subheader "Pagination demo (page-size=1, then fetch next page):"
  run_cmd "$FORGE_BIN" sagas list --page-size=1

  local cursor
  cursor=$("$FORGE_BIN" sagas list --page-size=1 2>/dev/null | awk '/^Next page: --cursor=/ {sub("--cursor=","",$3); print $3}')
  if [[ -n "$cursor" ]]; then
    info "Got cursor: $cursor"
    echo -e "${CYAN}  \$${RESET} $FORGE_BIN sagas list --page-size=1 --cursor=$cursor"
    "$FORGE_BIN" sagas list --page-size=1 --cursor="$cursor"
  else
    info "(Only 1 saga in list — cursor not available yet. Try after more sagas are created.)"
  fi
  pause

  subheader "Waiting for both sagas to complete..."
  wait_for_saga_state demo6a ready 90
  wait_for_saga_state demo6b ready 90

  run_cmd "$FORGE_BIN" list

  subheader "Pagination over completed sagas:"
  run_cmd "$FORGE_BIN" sagas list --page-size=1
  cursor=$("$FORGE_BIN" sagas list --page-size=1 2>/dev/null | awk '/^Next page: --cursor=/ {sub("--cursor=","",$3); print $3}')
  if [[ -n "$cursor" ]]; then
    echo -e "${CYAN}  \$${RESET} $FORGE_BIN sagas list --page-size=1 --cursor=$cursor"
    "$FORGE_BIN" sagas list --page-size=1 --cursor="$cursor"
  fi

  success "Demo 6 complete: ListSagas (filter, pagination) and GetSaga (step detail) demonstrated."
  pause
}

demo_07_abort_saga() {
  header "Demo 7 — AbortSaga (saga-conductor)"
  describe \
    "PROBLEM: Sometimes you want to stop a saga without triggering compensation." \
    "Use cases: 'the user cancelled before any compensatable resources were created'," \
    "'we know the environment will be manually cleaned up', or 'compensation itself" \
    "is broken and we just need to stop the retry loop immediately'." \
    "" \
    "SOLUTION: AbortSaga moves the saga directly to ABORTED — a terminal state —" \
    "without calling any compensate endpoints. It's the operator's escape hatch when" \
    "the normal compensation path is unavailable or undesirable." \
    "" \
    "HOW IT DIFFERS FROM COMPENSATION: Demo 2 showed FAILED (compensation ran)." \
    "This demo shows ABORTED (compensation was deliberately skipped). The step that" \
    "was mid-execution at abort time will show STEP_STATUS_RUNNING in the final state —" \
    "it was interrupted, not compensated." \
    "" \
    "NOTE: AbortSaga is not a rollback. Resources created by completed steps remain." \
    "Use it when you prefer explicit manual cleanup over automated compensation."
  info "Watch for: saga status=ABORTED, NO STEP_STATUS_COMPENSATED entries anywhere."
  diagram \
    "AbortSaga vs Compensation — two different exit paths" \
    "" \
    "  ABORT (this demo):                                                    " \
    "    RUNNING [vpc✓ rds→] ──► AbortSaga() ──► ABORTED                    " \
    "                                               │                        " \
    "                         NO compensation ◄─────┘  resources remain     " \
    "" \
    "  COMPENSATION (Demo 2, for comparison):                                " \
    "    RUNNING [... health✗] ──► COMPENSATING ──► FAILED                   " \
    "                                  │                                     " \
    "                    reverse compensations called ──► resources removed  " \
    "" \
    "  ABORTED is an operator escape hatch:                                  " \
    "    - Use when you prefer manual cleanup over automated compensation     " \
    "    - Use when compensation itself is broken                            " \
    "    - Use when no compensatable resources were created yet              "
  pause

  subheader "Starting dry-run saga..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo7 --dry-run &"
  "$FORGE_BIN" create --owner demo7 --dry-run &

  info "Sleeping 3s (rds step is now sleeping — good time to abort)..."
  sleep 3

  local prefix
  prefix=$(get_env_prefix_for_owner demo7 10)

  subheader "Aborting the saga (no compensation triggered):"
  run_cmd "$FORGE_BIN" sagas abort "$prefix"

  run_cmd "$FORGE_BIN" sagas list
  run_cmd "$FORGE_BIN" status "$prefix"

  info "Observe: saga status=SAGA_STATUS_ABORTED, NO STEP_STATUS_COMPENSATED entries."
  info "AbortSaga is an operator escape hatch — use when manual cleanup is preferred."
  success "Demo 7 complete: AbortSaga ABORTED terminal state, zero compensation."
  pause
}

demo_08_metrics_sse_dashboard() {
  header "Demo 8 — Prometheus Metrics + SSE Dashboard (saga-conductor)"
  describe \
    "PROBLEM: A saga orchestrator is a black box unless it exposes its internal state." \
    "You need two things: (1) historical counters for SLO dashboards (how many sagas" \
    "completed vs failed per hour?), and (2) real-time visibility for debugging" \
    "(which step is running right now? when did each step finish?)." \
    "" \
    "SOLUTION: saga-conductor exposes two pluggable observability interfaces:" \
    "" \
    "  Recorder interface → /metrics (Prometheus)" \
    "    saga_executions_total{status}   — counter per terminal state" \
    "    step_duration_seconds{step}     — histogram per step" \
    "    These metrics are scraped by Prometheus and power SLO dashboards." \
    "" \
    "  Observer interface → /dashboard/events (Server-Sent Events)" \
    "    Streams JSON events on every saga state transition." \
    "    The /dashboard HTML page consumes this SSE stream to render a live" \
    "    step-by-step progress view. SSE was chosen over WebSocket because it's" \
    "    unidirectional (conductor pushes) and works through HTTP/1.1 proxies." \
    "" \
    "Both interfaces are pluggable — saga-conductor only imports an interface type," \
    "not a specific Prometheus or SSE implementation."
  info "Watch for: saga_executions_total counter, then SSE events streaming step transitions."
  diagram \
    "OBSERVABILITY INTERFACES" \
    "" \
    "  saga-conductor" \
    "      │" \
    "      ├──► /metrics          (Prometheus — Recorder interface)         " \
    "      │      saga_executions_total{status='running'}   1               " \
    "      │      saga_executions_total{status='completed'} N               " \
    "      │      step_duration_seconds{step='vpc'}  histogram              " \
    "      │      (scraped by Prometheus; drives SLO dashboards)            " \
    "      │" \
    "      └──► /dashboard/events (SSE stream — Observer interface)         " \
    "             Each state transition pushes a JSON event:                " \
    "             data: {id, status:RUNNING,  steps:[{name:vpc, RUNNING}]}  " \
    "             data: {id, status:RUNNING,  steps:[{name:vpc, SUCCEEDED}]}" \
    "             data: {id, status:RUNNING,  steps:[{name:rds, RUNNING}]}  " \
    "             data: {id, status:COMPLETED,steps:[all SUCCEEDED]}        " \
    "             (consumed by /dashboard HTML for real-time visualisation) "
  pause

  subheader "Starting a saga in the background..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo8 --dry-run &"
  "$FORGE_BIN" create --owner demo8 --dry-run &

  info "Sleeping 2s for first steps to produce metric increments..."
  sleep 2

  subheader "Prometheus metrics — saga/step counters and durations:"
  run_pipe "curl -sf http://localhost:8081/metrics | grep -E '^(saga|step)_'" \
    "curl -sf http://localhost:8081/metrics | grep -E '^(saga|step)_' || true"
  pause

  subheader "Real-time SSE dashboard — streaming saga state changes for 6s:"
  info "In production, the /dashboard HTML page consumes this SSE stream."
  run_pipe "timeout 6 curl -sN http://localhost:8081/dashboard/events" \
    "timeout 6 curl -sN http://localhost:8081/dashboard/events || true"
  pause

  subheader "Waiting for saga to complete..."
  wait_for_saga_state demo8 ready 90

  subheader "Final metrics (completed counter incremented):"
  run_pipe "curl -sf http://localhost:8081/metrics | grep -E '^(saga|step)_'" \
    "curl -sf http://localhost:8081/metrics | grep -E '^(saga|step)_' || true"

  success "Demo 8 complete: Prometheus recorder and SSE observer interfaces demonstrated."
  pause
}

demo_09_graceful_drain() {
  header "Demo 9 — Graceful Drain (saga-conductor)"
  describe \
    "PROBLEM: Kubernetes rolling restarts send SIGTERM before spinning up a new pod." \
    "If the conductor exits immediately on SIGTERM, any in-flight saga is interrupted." \
    "The conductor's BoltDB records the saga as RUNNING, but no process is driving it." \
    "On restart, Resume() will re-pick it up — but there's a gap where no one is" \
    "retrying steps, and the gap can exceed step timeout_seconds." \
    "" \
    "SOLUTION: saga-conductor's Engine.Drain() performs a graceful shutdown:" \
    "  1. Stop accepting new saga executions (return UNAVAILABLE on new StartSaga calls)" \
    "  2. Wait for all currently in-flight step HTTP calls to complete (or timeout)" \
    "  3. Only then allow the process to exit" \
    "This eliminates the gap entirely for short-lived steps (< SHUTDOWN_DRAIN_SECONDS)." \
    "" \
    "HOW: We send SIGTERM at t=3s, when the rds step (8s total) has ~5s remaining." \
    "The conductor drains: it waits for rds to finish, then ec2, s3, identity, etc." \
    "We measure the drain duration — it should be ~14–18s, not <1s." \
    "After restart, Resume() scans BoltDB for any RUNNING sagas and re-drives them." \
    "" \
    "WHY DRAIN_SECONDS=0 in this script: We set SHUTDOWN_DRAIN_SECONDS=0 which means" \
    "'do not add extra wait time beyond the saga itself'. The saga completes during" \
    "the drain window naturally."
  info "Watch for: drain_secs >> 1 (proves saga was awaited, not dropped)."
  diagram \
    "GRACEFUL DRAIN + RESUME FLOW" \
    "" \
    "  t=0s   saga starts           [vpc✓  rds→  (5s remaining)]           " \
    "  t=3s   SIGTERM sent ──► Drain() begins:                              " \
    "           - stop accepting new StartSaga calls (return UNAVAILABLE)   " \
    "           - wait for in-flight rds step to complete...                " \
    "  t=8s   rds completes ──► ec2 starts                                  " \
    "  t=18s  all steps complete ──► saga COMPLETED                         " \
    "  t=18s  conductor exits cleanly  (drain_secs ≈ 15s)                  " \
    "" \
    "  t=18s  conductor restarted ──► Resume():                             " \
    "           scan BoltDB for RUNNING sagas ──► none (saga already done)  " \
    "           (if saga had NOT finished: Resume would re-drive from rds)  " \
    "" \
    "  Instant exit (<1s) would prove the drain was SKIPPED — saga dropped. " \
    "  15s+ exit proves drain WAITED — zero-downtime rolling restart.       "
  pause

  subheader "Starting dry-run saga..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo9 --dry-run &"
  "$FORGE_BIN" create --owner demo9 --dry-run &

  info "Sleeping 3s so rds step is mid-execution (~5s remaining in rds)..."
  sleep 3

  subheader "Sending SIGTERM to saga-conductor (graceful shutdown)..."
  info "Conductor will: flip ready=false → Engine.Drain() → wait for in-flight saga → exit"
  local drain_start
  drain_start=$(date +%s)
  kill -SIGTERM "$CONDUCTOR_PID"

  info "Waiting for conductor process to exit (should take ~14s while rds+remaining steps complete)..."
  wait "$CONDUCTOR_PID" 2>/dev/null || true
  CONDUCTOR_PID=0

  local drain_secs
  drain_secs=$(( $(date +%s) - drain_start ))
  success "Conductor exited after ${drain_secs}s drain (saga completed before shutdown — not dropped)."
  info "If drain was skipped, conductor would exit in <1s. ${drain_secs}s proves the saga was awaited."
  pause

  subheader "Restarting saga-conductor (Resume() scans for any RUNNING sagas)..."
  start_conductor
  wait_healthy "http://localhost:8081/health/live" 30

  subheader "Checking environment status:"
  wait_for_saga_state demo9 ready 30

  run_cmd "$FORGE_BIN" list
  local prefix
  prefix=$(get_env_prefix_for_owner demo9 10)
  run_cmd "$FORGE_BIN" status "$prefix"

  success "Demo 9 complete: Drain() preserved in-flight saga. Graceful rolling-restart safe."
  pause
}

demo_10_idempotency_key_and_timeout() {
  header "Demo 10 — Idempotency Key + Saga Timeout (saga-conductor)"
  describe \
    "PROBLEM: forge-api creates a saga by calling conductor's CreateSaga gRPC. If the" \
    "network drops between the RPC call and the response, forge-api doesn't know if" \
    "the saga was created. Retrying would create a duplicate saga, which would create" \
    "duplicate AWS resources." \
    "" \
    "SOLUTION: idempotency_key. forge-api sends idempotency_key='env-<uuid>' with" \
    "every CreateSaga. Conductor stores the key in BoltDB. If a second CreateSaga" \
    "arrives with the same key, conductor returns the existing saga ID instead of" \
    "creating a new one. This makes CreateSaga safely retryable." \
    "" \
    "SAGA TIMEOUT: saga_timeout_seconds=300 is a safety deadline. If the entire" \
    "provisioning takes longer than 5 minutes, conductor marks it FAILED rather than" \
    "letting it run indefinitely. This protects against hung steps." \
    "" \
    "PER-STEP CONFIGURATION: Different steps have different SLAs." \
    "  - vpc/s3/identity/config: fast steps → timeout=30s, max_retries=2, backoff=500ms" \
    "  - rds: slow (real polling): timeout=600s, max_retries=3, backoff=2000ms" \
    "  - health: one-shot check: timeout=60s, max_retries=1" \
    "This fine-grained config prevents fast steps from consuming all of rds's budget."
  info "Watch for: idempotency_key=env-<uuid> in conductor log, step config table."
  diagram \
    "IDEMPOTENCY KEY + PER-STEP CONFIGURATION" \
    "" \
    "  Idempotency key (CreateSaga deduplication):                          " \
    "    forge-api ──► CreateSaga(idempotency_key='env-<uuid>')             " \
    "                    │  first call:  creates saga_id=abc, stores key    " \
    "                    │  retry call:  finds key in BoltDB ──► returns abc" \
    "                    └► same saga_id either way — no duplicate saga     " \
    "" \
    "  Per-step config (timeout / max_retries / retry_backoff_ms):          " \
    "    Step      │ timeout │ retries │ backoff  │ reason                  " \
    "    ──────────┼─────────┼─────────┼──────────┼────────────────         " \
    "    vpc       │  30s    │   2     │  500ms   │ fast AWS API call        " \
    "    rds       │ 600s    │   3     │  2000ms  │ real RDS takes ~5min     " \
    "    ec2       │ 300s    │   3     │  2000ms  │ instance startup poll    " \
    "    s3        │  30s    │   2     │  500ms   │ fast AWS API call        " \
    "    identity  │  30s    │   2     │  500ms   │ svid-exchange gRPC       " \
    "    config    │  30s    │   2     │  500ms   │ S3 write + local file    " \
    "    health    │  60s    │   1     │  1000ms  │ one-shot DB check        " \
    "    registry  │  10s    │   1     │  200ms   │ BoltDB write (fast)      " \
    "    saga total │ 300s   │   —     │    —     │ 5-min overall deadline  "
  pause

  subheader "Provisioning an environment (blocking)..."
  run_cmd "$FORGE_BIN" create --owner demo10 --dry-run

  subheader "idempotency_key in conductor log:"
  info "forge-api sends idempotency_key='env-<uuid>' on every CreateSaga."
  info "Duplicate creates with the same key return the existing saga (network-retry safe)."
  run_pipe "grep -i 'idempotency' $CONDUCTOR_LOG | tail -5" \
    "grep -i 'idempotency' '$CONDUCTOR_LOG' 2>/dev/null | tail -5 || echo '  (enable DEBUG log level to see idempotency_key in conductor logs)'"

  subheader "Per-step configuration (timeout / max_retries / retry_backoff_ms):"
  cat <<'EOF'
  Step      │ timeout  │ max_retries │ backoff_ms
  ──────────┼──────────┼─────────────┼───────────
  vpc       │ 30s      │ 2           │ 500ms
  rds       │ 600s     │ 3           │ 2000ms  ← long poll: crash-recovery window
  ec2       │ 300s     │ 3           │ 2000ms
  s3        │ 30s      │ 2           │ 500ms
  identity  │ 30s      │ 2           │ 500ms
  config    │ 30s      │ 2           │ 500ms
  health    │ 60s      │ 1           │ 1000ms
  registry  │ 10s      │ 1           │ 200ms

  saga_timeout_seconds = 300 (5-minute deadline)
EOF

  subheader "Saga listed with its conductor ID:"
  run_cmd "$FORGE_BIN" sagas list

  success "Demo 10 complete: idempotency_key, saga_timeout_seconds, and per-step config demonstrated."
  pause
}

# ── Minikube demos ─────────────────────────────────────────────────────────────

demo_03_policy_lifecycle() {
  header "Demo 3 — svid-exchange Policy Lifecycle"
  describe \
    "PROBLEM: When an environment is provisioned, a new service-to-service trust" \
    "relationship is needed: the environment's app service needs to call its db-proxy." \
    "That trust should exist exactly as long as the environment exists — created when" \
    "the environment is created, deleted when it's torn down (or fails to provision)." \
    "" \
    "SOLUTION: svid-exchange's PolicyAdmin service manages named exchange policies." \
    "A policy says: 'SPIFFE identity X is allowed to exchange its SVID for a JWT" \
    "targeting Y, with scopes [read, write], max TTL 3600s'." \
    "" \
    "Step 5 (identity) of the env-forge saga:" \
    "  - Execute(): calls CreatePolicy(name='policy-env-<id8>', subject=env/app," \
    "               target=env/db-proxy, scopes=['read','write'])" \
    "  - Compensate(): calls DeletePolicy(name='policy-env-<id8>')" \
    "" \
    "This makes policies first-class citizens of the environment lifecycle, managed" \
    "as part of the same saga transaction — they're compensated along with everything" \
    "else if provisioning fails." \
    "" \
    "Source='dynamic' in the policy list means it was created via admin gRPC at" \
    "runtime (as opposed to Source='yaml' loaded from the policy file at startup)."
  info "Watch for: policy-env-<id8> appearing after provisioning, disappearing after compensation."
  diagram \
    "POLICY LIFECYCLE TIED TO ENVIRONMENT SAGA" \
    "" \
    "  Step 5 (identity) Execute():                                         " \
    "    svid-exchange admin gRPC ──► CreatePolicy(                         " \
    "      name    = 'policy-env-<id8>'                                     " \
    "      subject = spiffe://.../env-<id>/app                              " \
    "      target  = spiffe://.../env-<id>/db-proxy                         " \
    "      scopes  = [read, write]   max_ttl = 3600s                        " \
    "    )                                                                   " \
    "" \
    "  Step 5 (identity) Compensate():                                      " \
    "    svid-exchange admin gRPC ──► DeletePolicy(name='policy-env-<id8>') " \
    "" \
    "  ListPolicies before provision:  [forge-worker→forge-api  (yaml)]     " \
    "  ListPolicies after  provision:  [forge-worker→forge-api  (yaml),     " \
    "                                   policy-env-<id8>        (dynamic)]  " \
    "  ListPolicies after  compensation: [forge-worker→forge-api (yaml)]    "
  pause

  subheader "Initial policy list (YAML-sourced static policies only):"
  run_cmd "$FORGE_BIN" policies list --worker-url=http://localhost:9091
  pause

  subheader "Provisioning an environment (identity step will CreatePolicy)..."
  run_cmd "$FORGE_BIN" create --owner demo3a --dry-run

  subheader "Policy list after provisioning (dynamic policy now visible):"
  run_cmd "$FORGE_BIN" policies list --worker-url=http://localhost:9091
  info "Source=dynamic  → created via admin gRPC (not from YAML file)."
  pause

  subheader "Triggering compensation cascade (--fail-at-health deletes the policy)..."
  run_cmd "$FORGE_BIN" create --owner demo3b --dry-run --fail-at-health

  subheader "Policy list after compensation (demo3b's dynamic policy deleted):"
  run_cmd "$FORGE_BIN" policies list --worker-url=http://localhost:9091
  info "policy-env-<demo3b-id> is gone — identity step's Compensate() called DeletePolicy."

  success "Demo 3 complete: CreatePolicy on provision, DeletePolicy on compensation."
  pause
}

demo_04_jwt_token_exchange() {
  header "Demo 4 — JWT Token Exchange (svid-exchange)"
  describe \
    "PROBLEM: forge-worker calls forge-api to read/write environment state. How does" \
    "forge-api know the caller is actually forge-worker and not a rogue process?" \
    "Shared secrets are bad (they leak). IP allowlists don't work in Kubernetes" \
    "(pods move). Service account tokens are Kubernetes-specific and don't travel." \
    "" \
    "SOLUTION: SPIFFE + svid-exchange. SPIRE is a workload identity platform:" \
    "  1. forge-worker attests its identity to the SPIRE agent using the k8s_psat" \
    "     attestor (its service account + node info, cryptographically verified)." \
    "  2. SPIRE issues forge-worker an X.509 SVID: a short-lived cert encoding" \
    "     its SPIFFE ID (spiffe://cluster.local/ns/default/sa/forge-worker)." \
    "  3. forge-worker presents its SVID to svid-exchange over mTLS. svid-exchange" \
    "     verifies the SVID's chain of trust against the SPIRE CA bundle." \
    "  4. svid-exchange checks if a policy allows this SVID to get a token for" \
    "     the target (forge-api). It mints an ES256 JWT with the allowed scopes." \
    "  5. forge-worker attaches the JWT as 'Authorization: Bearer <token>'." \
    "  6. forge-api verifies the JWT's signature against svid-exchange's JWKS endpoint." \
    "" \
    "WHY NOT JUST USE MTLS BETWEEN SERVICES? mTLS requires both sides to present certs." \
    "JWTs are simpler to pass through HTTP middleware and support scope-based access" \
    "control (Demo 13) and revocation (Demo 12) that mTLS alone cannot provide."
  info "Watch for: 'JWT token exchange enabled' and token_id fields in forge-worker logs."
  diagram \
    "ZERO-TRUST TOKEN EXCHANGE FLOW (per step call)" \
    "" \
    "  forge-worker            SPIRE agent        svid-exchange      forge-api" \
    "       │                      │                    │                │   " \
    "       │─ FetchX509SVID() ───►│                    │                │   " \
    "       │◄─ X.509 SVID ────────│                    │                │   " \
    "       │  (EC cert, SPIFFE ID = .../sa/forge-worker)                │   " \
    "       │                                           │                │   " \
    "       │─ Exchange(SVID, target=forge-api) [mTLS]─►│                │   " \
    "       │  svid-exchange verifies SVID chain of trust                │   " \
    "       │  checks policy: forge-worker → forge-api allowed?          │   " \
    "       │◄─ JWT{sub=forge-worker, aud=forge-api,    │                │   " \
    "       │       scopes=[env:read,env:write], jti=X} │                │   " \
    "       │                                                            │   " \
    "       │─ GET /internal/envs/{id}  Authorization: Bearer JWT ──────►│   " \
    "       │                                    verify sig + aud + scope│   " \
    "       │◄─────────────────────────────── 200 OK  env JSON ──────────│   "
  pause

  subheader "Starting provisioning and watching forge-worker logs for JWT exchange..."
  subheader "forge-worker logs (streaming in background — look for 'JWT exchanged' and token_id):"
  echo -e "${CYAN}  \$${RESET} kubectl logs -f -l app=forge-worker -n default &"
  kubectl logs -f -l app=forge-worker -n default 2>/dev/null &
  local log_pid=$!
  pause

  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo4 --dry-run"
  "$FORGE_BIN" create --owner demo4 --dry-run
  kill "$log_pid" 2>/dev/null || true
  wait "$log_pid" 2>/dev/null || true

  run_cmd "$FORGE_BIN" list

  subheader "SPIRE workload entries (confirm forge-worker has SVID):"
  run_pipe "kubectl exec -n spire statefulset/spire-server -- /opt/spire/bin/spire-server entry show -socketPath /run/spire/sockets/api.sock 2>/dev/null | grep SPIFFE" \
    "kubectl exec -n spire statefulset/spire-server -- /opt/spire/bin/spire-server entry show -socketPath /run/spire/sockets/api.sock 2>/dev/null | grep 'SPIFFE ID' || true"

  success "Demo 4 complete: SPIFFE SVID → ES256 JWT → forge-api Bearer token validation."
  pause
}

demo_11_policy_lifecycle_reload() {
  header "Demo 11 — Policy Lifecycle + ListPolicies + ReloadPolicy (svid-exchange)"
  describe \
    "PROBLEM: Policy management in a real system has two workflows:" \
    "  (a) Static policies for long-lived service relationships (e.g. forge-worker →" \
    "      forge-api) — defined in a YAML file, loaded at startup." \
    "  (b) Dynamic policies for ephemeral resources (e.g. env-<id>/app → db-proxy) —" \
    "      created/deleted at runtime via admin gRPC." \
    "When the YAML file changes (new service added, scope updated), you need to" \
    "apply those changes without restarting svid-exchange and interrupting all" \
    "in-flight token exchanges." \
    "" \
    "SOLUTION:" \
    "  ListPolicies: returns both yaml-sourced and dynamic policies with source tag." \
    "  ReloadPolicy: re-reads the YAML file, merges with existing dynamic policies." \
    "    Dynamic policies are preserved across reload; YAML policies are re-evaluated." \
    "" \
    "grpc_reflection=true: svid-exchange registers the gRPC reflection service, so" \
    "grpcurl can list services and call methods without needing the proto file locally." \
    "This makes svid-exchange's admin API self-documenting and explorable." \
    "" \
    "The forge-worker admin proxy: forge-worker exposes an HTTP endpoint that proxies" \
    "policy/token admin calls to svid-exchange's admin gRPC using its SPIFFE mTLS" \
    "credentials — so the forge CLI doesn't need a SPIFFE identity of its own."
  info "Watch for: source=yaml vs source=dynamic in policy list; grpcurl service discovery."
  diagram \
    "POLICY SOURCES + RELOAD FLOW" \
    "" \
    "  svid-exchange startup:                                               " \
    "    policy.yaml ──► load ──► memory{source=yaml}                       " \
    "" \
    "  identity step Execute():                                             " \
    "    CreatePolicy() ──► memory{source=dynamic} + BoltDB                " \
    "" \
    "  ReloadPolicy():                                                      " \
    "    policy.yaml ──► re-read ──► merge                                  " \
    "                     yaml policies updated  ─────────────────► memory  " \
    "                     dynamic policies preserved (not from yaml) ──────►│" \
    "" \
    "  ListPolicies result:                                                  " \
    "    NAME                      SOURCE   SUBJECT          TARGET          " \
    "    forge-worker→forge-api    yaml     .../forge-worker .../forge-api   " \
    "    policy-env-<id8>          dynamic  .../env/app      .../env/db-proxy" \
    "" \
    "  gRPC reflection (grpc_reflection=true):                              " \
    "    grpcurl list localhost:8082 ──► admin.v1.PolicyAdmin               " \
    "    (no proto file needed — server describes itself)                   "
  pause

  subheader "Provisioning an environment (triggers CreatePolicy + ReloadPolicy in identity step)..."
  run_cmd "$FORGE_BIN" create --owner demo11 --dry-run

  subheader "forge-worker identity step logs:"
  run_pipe "kubectl logs -l app=forge-worker -n default | grep -A3 identity" \
    "kubectl logs -l app=forge-worker -n default 2>/dev/null | grep -A3 'identity' | tail -20 || true"
  pause

  subheader "List all policies via forge-worker admin proxy:"
  run_cmd "$FORGE_BIN" policies list --worker-url=http://localhost:9091

  subheader "gRPC reflection — discover services without proto files:"
  if command -v grpcurl >/dev/null 2>&1; then
    run_pipe "grpcurl -plaintext localhost:8082 list" \
      "grpcurl -plaintext localhost:8082 list 2>/dev/null || true"
    run_pipe "grpcurl -plaintext localhost:8082 admin.v1.PolicyAdmin/ListPolicies" \
      "grpcurl -plaintext localhost:8082 admin.v1.PolicyAdmin/ListPolicies 2>/dev/null || true"
  else
    warn "grpcurl not found — skipping gRPC reflection demo (install: go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest)"
  fi
  pause

  subheader "Reload YAML policies without restarting svid-exchange (ReloadPolicy RPC):"
  run_cmd "$FORGE_BIN" policies reload --worker-url=http://localhost:9091

  subheader "Policy list after reload (dynamic policies preserved, YAML re-merged):"
  run_cmd "$FORGE_BIN" policies list --worker-url=http://localhost:9091

  success "Demo 11 complete: CreatePolicy, ListPolicies, ReloadPolicy all demonstrated."
  pause
}

demo_12_token_revocation() {
  header "Demo 12 — Token Revocation (svid-exchange)"
  describe \
    "PROBLEM: JWTs are stateless — once issued, they're valid until their exp claim." \
    "If a token is leaked (logged accidentally, stolen from memory), the attacker can" \
    "use it for its full TTL (up to 3600s). You need a way to invalidate a specific" \
    "token before it expires." \
    "" \
    "SOLUTION: svid-exchange maintains a revocation list in BoltDB. Each issued token" \
    "has a unique jti (JWT ID) claim. RevokeToken(jti, expires_at) adds the jti to" \
    "the revocation store. When validating any incoming token, svid-exchange checks" \
    "the jti against the revocation list and rejects matches." \
    "" \
    "The expires_at parameter tells svid-exchange when to purge the revocation record" \
    "(once the token's natural expiry passes, it would be rejected by exp anyway —" \
    "no need to keep the revocation entry forever). This keeps the revocation store" \
    "bounded in size." \
    "" \
    "BoltDB persistence: the revocation list survives svid-exchange restarts. An" \
    "in-memory store would allow a revoked token to become valid again after restart." \
    "" \
    "HOW: We extract a jti from the svid-exchange audit log, revoke it via forge-worker" \
    "proxy, then confirm it appears in ListRevokedTokens."
  info "Watch for: jti extracted from logs, revocation persisted, ListRevokedTokens output."
  diagram \
    "TOKEN REVOCATION FLOW" \
    "" \
    "  Normal flow:                                                          " \
    "    svid-exchange issues JWT(jti='abc-xyz', exp=now+3600)               " \
    "    forge-api validates ──► OK                                          " \
    "" \
    "  After RevokeToken(jti='abc-xyz', expires_at=now+3600):               " \
    "    BoltDB revocation store: { 'abc-xyz': expires_at }                 " \
    "    forge-worker presents same JWT ──► svid-exchange checks jti         " \
    "                                   ──► FOUND in revocation list         " \
    "                                   ──► REJECTED (even before exp)       " \
    "" \
    "  Persistence across restart:                                           " \
    "    svid-exchange restart ──► BoltDB reloaded ──► token still rejected  " \
    "    (in-memory store would let revoked token become valid again)        " \
    "" \
    "  Auto-cleanup:                                                         " \
    "    when expires_at passes ──► record purged (token expired anyway)    "
  pause

  subheader "Provisioning an environment (forge-worker exchanges JWTs during each step)..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo12 --dry-run &"
  "$FORGE_BIN" create --owner demo12 --dry-run &
  local create_pid=$!
  sleep 5

  subheader "Extracting a JWT token_id (jti) from svid-exchange audit log:"
  local jti
  jti=$(kubectl logs -l app=svid-exchange -n default 2>/dev/null \
    | grep '"token_id"' | tail -1 \
    | sed 's/.*"token_id":"\([^"]*\)".*/\1/' 2>/dev/null || true)

  if [[ -n "$jti" && "$jti" != *"token_id"* ]]; then
    info "Found JTI: $jti"
  else
    warn "Could not auto-extract JTI. Using a placeholder for demo."
    jti="demo-placeholder-jti-$(date +%s)"
    info "In production: kubectl logs -l app=svid-exchange | grep token_id"
  fi

  local expires_at
  # GNU date
  expires_at=$(date -d '+1 hour' +%s 2>/dev/null || date -v +1H +%s 2>/dev/null || echo "$(( $(date +%s) + 3600 ))")

  subheader "Revoking the token (persisted to BoltDB — survives svid-exchange restart):"
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN tokens revoke $jti --worker-url=http://localhost:9091 --expires-at=$expires_at"
  "$FORGE_BIN" tokens revoke "$jti" \
    --worker-url=http://localhost:9091 \
    --expires-at="$expires_at" || warn "Revoke returned error (placeholder JTI not found in svid-exchange)"

  subheader "List all revoked tokens:"
  run_cmd "$FORGE_BIN" tokens revoked --worker-url=http://localhost:9091

  # Wait for the background forge create to finish (avoids polling by owner
  # which would match stale failed envs from previous demo runs in BoltDB).
  wait "$create_pid" || true
  success "Demo 12 complete: RevokeToken and ListRevokedTokens with BoltDB persistence."
  pause
}

demo_13_scope_enforcement() {
  header "Demo 13 — Scope Enforcement (svid-exchange client library)"
  describe \
    "PROBLEM: A token that allows forge-worker to read env state should not also allow" \
    "it to write to an arbitrary unrelated endpoint. Token-based auth is only as strong" \
    "as the scope checks that enforce it. Without scope checks, any valid JWT grants" \
    "full access to all endpoints — the token is a skeleton key." \
    "" \
    "SOLUTION: svid-exchange's client library provides:" \
    "  NewMiddleware(verifier, audience) — HTTP middleware that:" \
    "    1. Extracts the Bearer JWT from the request" \
    "    2. Verifies signature and expiry against the JWKS" \
    "    3. Checks the audience claim matches" \
    "    4. Stores the parsed claims in the request context" \
    "  ClaimsFromContext(ctx) — retrieves the verified claims inside a handler" \
    "  HasScope(claims, 'env:read') — checks if a single scope was granted" \
    "  HasAllScopes(claims, 'env:read', 'env:write') — checks multiple scopes" \
    "" \
    "forge-api's internal routes are gated:" \
    "  GET /internal/envs/{id}  → requires env:read   (reading state is less privileged)" \
    "  PUT /internal/envs/{id}  → requires env:read AND env:write  (write needs both)" \
    "" \
    "The forge-worker-to-forge-api policy grants ['env:read', 'env:write'], so normal" \
    "provisioning passes. A service with only env:read could inspect but not modify."
  info "Watch for: forge-api log lines showing 'authorized' with scope=env:read env:write."
  diagram \
    "SCOPE ENFORCEMENT IN forge-api" \
    "" \
    "  JWT claims: { sub: forge-worker, scopes: [env:read, env:write] }     " \
    "" \
    "  GET /internal/envs/{id}  (read env state from BoltDB)                " \
    "    NewMiddleware ──► parse JWT ──► ClaimsFromContext(ctx)              " \
    "    HasScope(claims, 'env:read') ──► TRUE ──► handler executes ──► 200 " \
    "" \
    "  PUT /internal/envs/{id}  (write env state to BoltDB)                 " \
    "    NewMiddleware ──► parse JWT ──► ClaimsFromContext(ctx)              " \
    "    HasAllScopes(claims, 'env:read', 'env:write') ──► TRUE ──► 200     " \
    "" \
    "  Hypothetical token with only env:read:                               " \
    "    PUT ──► HasAllScopes('env:read','env:write') ──► FALSE ──► 403     " \
    "" \
    "  Why both scopes on PUT? Read-before-write pattern — handler reads    " \
    "  the current env state before merging the update. env:read enforces   " \
    "  that the caller is also allowed to see the existing state.           "
  pause

  subheader "Starting provisioning and watching forge-api logs for scope checks..."
  echo -e "${CYAN}  \$${RESET} $FORGE_BIN create --owner demo13 --dry-run &"
  "$FORGE_BIN" create --owner demo13 --dry-run &
  local create_pid=$!
  sleep 5

  subheader "forge-api authorization log lines (scope enforcement):"
  run_pipe "kubectl logs -l app=forge-api -n default | grep -iE '(scope|authorized|forbidden|denied)' | tail -15" \
    "kubectl logs -l app=forge-api -n default 2>/dev/null | grep -iE '(scope|authorized|forbidden|denied)' | tail -15 || true"

  info "GET /internal/envs/{id}  → requires env:read  (HasScope)"
  info "PUT /internal/envs/{id}  → requires env:read + env:write  (HasAllScopes)"
  info "Both granted via the forge-worker-to-forge-api policy."
  pause

  wait "$create_pid" || true
  run_cmd "$FORGE_BIN" list

  success "Demo 13 complete: scope enforcement via svid-exchange NewMiddleware demonstrated."
  pause
}

demo_14_key_rotation_rate_limiting() {
  header "Demo 14 — Key Rotation + Rate Limiting (svid-exchange)"
  describe \
    "KEY ROTATION — why it matters:" \
    "  Cryptographic best practice requires rotating signing keys periodically." \
    "  If a private key is compromised, all tokens it signed are compromised." \
    "  Rotation limits the blast radius: a leaked key can only sign tokens for" \
    "  its rotation window, not forever." \
    "" \
    "  svid-exchange generates a new ES256 key pair every key_rotation_interval (1h)." \
    "  The old key remains in the JWKS endpoint until all tokens it signed expire." \
    "  forge-api auto-refreshes JWKS every 5 minutes — it picks up new keys silently." \
    "  forge-worker's background refresh (at 80% TTL) re-fetches tokens with the new key." \
    "  Result: zero-downtime key rotation, no token invalidation." \
    "" \
    "RATE LIMITING — why it matters:" \
    "  Without rate limiting, a compromised service (or a bug) could call Exchange()" \
    "  thousands of times per second, generating tokens that overwhelm downstream" \
    "  services or exhaust SPIRE's SVID signing capacity." \
    "  svid-exchange uses a token bucket (rate_limit_rps + burst) per client identity." \
    "  Excess calls return gRPC RESOURCE_EXHAUSTED immediately." \
    "" \
    "AUDIT HMAC — why it matters:" \
    "  svid-exchange logs every Exchange() call with structured JSON including subject," \
    "  target, scopes, and jti. If AUDIT_HMAC_KEY is set, each log line includes an" \
    "  HMAC of the event fields. This makes the audit log tamper-evident — you can" \
    "  verify that no log lines were edited or deleted after the fact." \
    "" \
    "PORT NOTE: svid-exchange's health/JWKS runs on port 8081 in-cluster, but we" \
    "port-forward it to 8083 locally to avoid conflict with conductor's 8081."
  info "Watch for: JWKS keys array, audit log HMAC field, rotation log entries."
  diagram \
    "KEY ROTATION + RATE LIMITING + AUDIT" \
    "" \
    "  Key rotation timeline (key_rotation_interval=1h):                    " \
    "    t=0h  k1 active   JWKS=[k1]      tokens signed with k1             " \
    "    t=1h  k2 generated JWKS=[k1,k2]  new tokens signed with k2         " \
    "          k1 kept in JWKS until all k1-signed tokens expire (max 1h)   " \
    "    t=2h  all k1 tokens expired  JWKS=[k2]  k1 removed                 " \
    "                                                                        " \
    "    forge-api JWKS refresh (every 5min) ──► always has current keys    " \
    "    forge-worker token refresh (80% TTL) ──► re-fetches before expiry  " \
    "    Result: zero downtime, no token invalidation during rotation        " \
    "" \
    "  Rate limiting (token bucket per client identity):                     " \
    "    rate_limit_rps=50  burst=10                                         " \
    "    request 1-10: admitted immediately (burst)                          " \
    "    request 11+:  admitted at 50/s rate  or RESOURCE_EXHAUSTED         " \
    "" \
    "  Audit log (AUDIT_HMAC_KEY set → tamper-evident):                     " \
    "    {event:exchange, sub:forge-worker, scopes:[env:read,env:write],     " \
    "     jti:abc, hmac:<sha256-of-fields>}                                  " \
    "    verifying hmac proves no log lines were edited post-facto           "
  pause

  subheader "svid-exchange startup logs (key rotation + rate limit config):"
  run_pipe "kubectl logs -l app=svid-exchange -n default | grep -iE '(rotation|rate_limit|audit|hmac)' | head -15" \
    "kubectl logs -l app=svid-exchange -n default 2>/dev/null | grep -iE '(rotation|rate_limit|audit|hmac)' | head -15 || true"
  pause

  subheader "JWKS endpoint — public keys currently active (current + previous during rotation window):"
  info "forge-api auto-refreshes JWKS every 5 minutes. forge-worker re-fetches tokens at 80% TTL."
  info "Zero-downtime key rotation: old tokens remain valid until their exp claim."
  run_pipe "curl -sf http://localhost:8083/jwks | python3 -m json.tool" \
    "curl -sf http://localhost:8083/jwks 2>/dev/null | python3 -m json.tool 2>/dev/null || curl -sf http://localhost:8083/jwks 2>/dev/null || warn 'JWKS endpoint unreachable (port-forward: svid-exchange:8081→8083)'"
  pause

  subheader "Audit log sample (structured JSON with optional HMAC tamper-evidence):"
  run_pipe "kubectl logs -l app=svid-exchange -n default | grep 'token.exchange' | tail -5" \
    "kubectl logs -l app=svid-exchange -n default 2>/dev/null | grep 'token.exchange' | tail -5 || true"

  info "Rate limiting: RESOURCE_EXHAUSTED gRPC status code when rate_limit_rps exceeded."
  info "key_rotation_interval='1h' in server.yaml ConfigMap triggers automatic key rotation."

  success "Demo 14 complete: key_rotation_interval, rate limiting, JWKS, and audit logging."
  pause
}

# ─── Demo Dispatcher ─────────────────────────────────────────────────────────

run_demo_by_number() {
  case "$1" in
    1)  demo_01_crash_recovery ;;
    2)  demo_02_compensation_cascade ;;
    3)  demo_03_policy_lifecycle ;;
    4)  demo_04_jwt_token_exchange ;;
    5)  demo_05_dead_lettering ;;
    6)  demo_06_list_sagas_step_detail ;;
    7)  demo_07_abort_saga ;;
    8)  demo_08_metrics_sse_dashboard ;;
    9)  demo_09_graceful_drain ;;
    10) demo_10_idempotency_key_and_timeout ;;
    11) demo_11_policy_lifecycle_reload ;;
    12) demo_12_token_revocation ;;
    13) demo_13_scope_enforcement ;;
    14) demo_14_key_rotation_rate_limiting ;;
    *)  fail "Unknown demo number: $1 (valid: 1-14)" ;;
  esac
}

# ─── Orchestrators ────────────────────────────────────────────────────────────

run_local_demos() {
  header "Phase 1 — Local Mode (saga-conductor features)"
  info "Services: saga-conductor, forge-api, forge-worker — all local binaries, no Kubernetes."
  info "Covers: Demos 1 (crash recovery), 2 (compensation), 5 (dead-letter),"
  info "        6 (list/paginate), 7 (abort), 8 (metrics/SSE), 9 (drain), 10 (idempotency)."
  echo ""

  show_overview
  pause

  check_prereqs_local

  build_if_needed "$CONDUCTOR_BIN"    "$CONDUCTOR_DIR" "saga-conductor"
  build_if_needed "$FORGE_API_BIN"    "$SCRIPT_DIR"    "env-forge"
  build_if_needed "$FORGE_BIN"        "$SCRIPT_DIR"    "env-forge"

  start_all_local

  local local_demos=(1 2 5 6 7 8 9 10)
  local i
  for i in "${!local_demos[@]}"; do
    local n="${local_demos[$i]}"
    [[ -n "$DEMO_ONLY" && "$DEMO_ONLY" != "$n" ]] && continue
    run_demo_by_number "$n"
    # Reset between demos (not after the last one)
    if [[ $((i + 1)) -lt ${#local_demos[@]} && -z "$DEMO_ONLY" ]]; then
      reset_services_between_demos
    fi
  done

  header "Local demos complete."
}

run_minikube_demos() {
  header "Phase 2 — Minikube Mode (svid-exchange + zero-trust JWT features)"
  info "Services: deployed to minikube with SPIRE workload identity."
  info "Covers: Demos 3 (policy lifecycle), 4 (JWT exchange), 11 (reload), 12 (revoke),"
  info "        13 (scope enforcement), 14 (key rotation + rate limiting)."
  echo ""

  show_overview
  pause

  check_prereqs_minikube
  setup_minikube_port_forwards

  local minikube_demos=(3 4 11 12 13 14)
  for n in "${minikube_demos[@]}"; do
    [[ -n "$DEMO_ONLY" && "$DEMO_ONLY" != "$n" ]] && continue
    run_demo_by_number "$n"
  done

  header "Minikube demos complete."
}

# ─── Argument Parsing + Main ──────────────────────────────────────────────────

usage() {
  cat <<EOF

  env-forge Demo Script — covers all 14 demo moments

  Usage: $0 [OPTIONS]

  Options:
    --local      Run local-mode demos (1,2,5,6,7,8,9,10)  [default]
    --minikube   Run minikube-mode demos (3,4,11,12,13,14)
    --all        Run both phases (local first, then minikube)
    --auto       Non-interactive (no pause prompts)
    --demo N     Run only demo N (1-14); infers mode from demo number
    -h, --help   Show this help

  Local mode requirements:
    mise, Go 1.26.1, free ports: 8080 8081 9090 9091
    saga-conductor at: ~/go_projects/saga-conductor

  Minikube mode additional requirements:
    minikube (running), kubectl, SPIRE deployed, all 4 services deployed
    See: docs/minikube-setup.md

  Demo moments covered:
    Local:     1 crash-recovery  2 compensation  5 dead-letter  6 list-sagas
               7 abort           8 metrics/SSE   9 drain        10 idempotency
    Minikube:  3 policy-crud     4 jwt-exchange  11 reload
               12 revoke-token   13 scope-check  14 key-rotation

EOF
}

main() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --local)    RUN_MODE="local" ;;
      --minikube) RUN_MODE="minikube" ;;
      --all)      RUN_MODE="all" ;;
      --auto)     AUTO_MODE="true" ;;
      --demo)
        [[ $# -gt 1 ]] || fail "--demo requires a number argument"
        DEMO_ONLY="$2"
        shift
        # Infer mode from demo number
        case "$DEMO_ONLY" in
          3|4|11|12|13|14) RUN_MODE="minikube" ;;
          *)               RUN_MODE="local" ;;
        esac
        ;;
      -h|--help) usage; exit 0 ;;
      *) fail "Unknown option: $1  (use --help for usage)" ;;
    esac
    shift
  done

  echo ""
  echo -e "${BOLD}env-forge demo — saga-conductor + svid-exchange${RESET}"
  echo -e "${DIM}Mode: ${RUN_MODE}  |  Auto: ${AUTO_MODE}  |  Demo: ${DEMO_ONLY:-all}${RESET}"
  echo ""

  case "$RUN_MODE" in
    local)    run_local_demos ;;
    minikube) run_minikube_demos ;;
    all)
      run_local_demos
      echo ""
      if [[ "$AUTO_MODE" == "false" ]]; then
        echo -e "${DIM}Local demos complete. Press ENTER to continue with minikube demos${RESET}"
        echo -e "${DIM}(requires minikube running with all services deployed — see docs/minikube-setup.md)${RESET}"
        read -r
      fi
      run_minikube_demos
      ;;
  esac

  header "All demos complete."
  info "Logs saved in: $LOG_DIR"
}

main "$@"
