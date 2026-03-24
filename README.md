# env-forge

Demo platform. Provisions isolated AWS developer environments (VPC, RDS, EC2, S3) via saga orchestration — every step is reversible. Demonstrates **saga-conductor** (distributed transactions) and **svid-exchange** (zero-trust token exchange) working together across independently-attested services.

## Architecture

```
forge create --owner alice --dry-run
    │
    │  POST /envs/create
    ▼
forge-api (port 9090)              ← SPIFFE ID: .../ns/default/sa/forge-api
    │  gRPC CreateSaga + StartSaga
    ▼
saga-conductor (orchestrator)
    │  POST /steps/{name}
    ▼
forge-worker (port 9091)           ← SPIFFE ID: .../ns/default/sa/forge-worker
    │
    ├── [SPIRE Workload API → SVID]
    ├── [svid-exchange:8080 Exchange(target=forge-api) → JWT]
    │
    │  GET|PUT /internal/envs/{id}   Authorization: Bearer <JWT>
    ▼
forge-api validates JWT via JWKS → BoltDB read/write
    │
    ├── Step 1: vpc       — VPC + subnets + security groups
    ├── Step 2: rds       — RDS postgres db.t3.micro
    ├── Step 3: ec2       — t3.micro with SPIRE agent
    ├── Step 4: s3        — versioned S3 bucket
    ├── Step 5: identity  — svid-exchange exchange policy (app → db-proxy)
    ├── Step 6: config    — config.json → S3 + local .env
    ├── Step 7: health    — SELECT 1, SPIRE health check
    └── Step 8: registry  — mark environment "ready" in BoltDB
```

Every step stores its output in BoltDB (keyed by env_id, owned by forge-api). Each step checks `IsAlreadyDone` before executing, so crash recovery after any failure is automatic.

## Services

| Service | Purpose | Port |
|---------|---------|------|
| **forge-api** | User API + BoltDB owner + JWT validation | 9090 (HTTP) |
| **forge-worker** | Saga step executor + SPIFFE attestation | 9091 (HTTP) |
| **saga-conductor** | Saga orchestrator | 8080 (gRPC) |
| **svid-exchange** | Zero-trust token exchange | 8080 (gRPC data), 8082 (gRPC admin) |
| **SPIRE server** | Workload identity CA | 8081 (gRPC) |
| **SPIRE agent** | Workload attestation | DaemonSet |

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/docs/start/)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [mise](https://mise.jdx.dev/) (installs Go 1.26.1 automatically)

## Local Quick Start (no Kubernetes)

Run all services as local binaries for the fastest dev loop. forge-worker calls forge-api without JWT in this mode (`SPIFFE_ENDPOINT_SOCKET` unset).

```bash
# Build
mise install && make build
cd ~/go_projects/saga-conductor && make build

# Terminal A: saga-conductor
cd ~/go_projects/saga-conductor
DB_PATH=/tmp/saga.db ./bin/saga-conductor

# Terminal B: forge-api (JWT validation disabled — no SVIDEXCHANGE_JWKS_URL)
cd ~/go_projects/env-forge
DB_PATH=/tmp/forge.db \
  CONDUCTOR_ADDR=localhost:8080 \
  FORGE_WORKER_URL=http://localhost:9091 \
  ./bin/forge-api

# Terminal C: forge-worker (dev mode — no SPIFFE socket)
cd ~/go_projects/env-forge
FORGE_API_URL=http://localhost:9090 \
  ./bin/forge-worker

# Terminal D: provision
./bin/forge create --owner alice --dry-run
# ✓ Environment ready in ~20s, all 8 steps completed
```

## Minikube Setup (full SPIRE + JWT token exchange)

All manifests use `imagePullPolicy: Never` — images must be pre-loaded into minikube. Minikube with the Docker driver cannot pull from external registries in offline or restricted environments.

```bash
# Start minikube
minikube start

# Pre-load all required images (minikube can't pull from external registries)
minikube image load ghcr.io/spiffe/spire-server:1.9.6
minikube image load ghcr.io/spiffe/spire-agent:1.9.6
minikube image load cgr.dev/chainguard/kubectl:latest
minikube image load ghcr.io/ngaddam369/saga-conductor:latest
minikube image load ghcr.io/ngaddam369/svid-exchange:latest

# Build and load forge images
docker build -t ghcr.io/ngaddam369/env-forge-api:latest .
docker build -f Dockerfile.worker -t ghcr.io/ngaddam369/env-forge-worker:latest .
minikube image load ghcr.io/ngaddam369/env-forge-api:latest
minikube image load ghcr.io/ngaddam369/env-forge-worker:latest

# Deploy SPIRE — create the bundle ConfigMap first, then server, then agent
kubectl apply -f k8s/spire/spire-server.yaml
kubectl create configmap spire-bundle -n spire 2>/dev/null || true
kubectl wait --for=condition=Ready pod -n spire -l app=spire-server --timeout=120s
kubectl apply -f k8s/spire/spire-agent.yaml
kubectl wait --for=condition=Ready pod -n spire -l app=spire-agent --timeout=120s

# Register SPIRE workload entries (svid-exchange, forge-api, forge-worker)
bash scripts/register-spire-entries.sh

# Deploy saga-conductor and svid-exchange
kubectl apply -f k8s/saga-conductor.yaml
kubectl apply -f k8s/svid-exchange.yaml
kubectl wait --for=condition=Ready pod -l app=saga-conductor --timeout=120s
kubectl wait --for=condition=Ready pod -l app=svid-exchange --timeout=120s

# Deploy forge-api and forge-worker
kubectl apply -f k8s/forge-api.yaml
kubectl apply -f k8s/forge-worker.yaml
kubectl wait --for=condition=Ready pod -l app=forge-api --timeout=120s
kubectl wait --for=condition=Ready pod -l app=forge-worker --timeout=120s

# (Optional) Create AWS credentials secret for real provisioning
kubectl create secret generic aws-creds \
  --from-literal=AWS_ACCESS_KEY_ID=<key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret> \
  --from-literal=AWS_REGION=us-east-1

# Port-forward and test
kubectl port-forward svc/forge-api 9090:9090 &
./bin/forge create --owner alice --dry-run
# → Environment <id> — status: ready  (all 8 steps + JWT token exchange)
```

> **Note:** `spire-bundle` must exist before the server starts — it's where the server publishes its CA bundle. The agent reads it as its bootstrap trust. Always create it empty before the first `kubectl apply -f k8s/spire/spire-server.yaml`.

## CLI Usage

```bash
# Build the CLI
mise install && make build

# Provision (dry-run, no AWS needed)
./bin/forge create --owner alice --dry-run

# Check status — shows step-level detail + failed_step + error_detail
./bin/forge status <env-id-prefix>

# List environments
./bin/forge list

# ── saga-conductor commands ──────────────────────────────────────────────────

# List sagas (uses ListSagas RPC with status filter and pagination)
./bin/forge sagas list
./bin/forge sagas list --status=RUNNING
./bin/forge sagas list --page-size=10 --cursor=<next-page-token>

# Abort a running saga (AbortSaga — no compensation triggered)
./bin/forge sagas abort <env-id-prefix>

# ── svid-exchange commands ───────────────────────────────────────────────────

# List exchange policies (YAML-sourced + dynamic)
# Admin commands route through forge-worker's HTTP proxy (SPIFFE mTLS to svid-exchange admin)
./bin/forge policies list --worker-url=http://localhost:9091

# Reload YAML policies without restart (ReloadPolicy)
./bin/forge policies reload --worker-url=http://localhost:9091

# Revoke a JWT by its jti claim (RevokeToken)
./bin/forge tokens revoke <token-jti> --worker-url=http://localhost:9091 --expires-at=<unix-ts>

# List all revoked tokens (ListRevokedTokens)
./bin/forge tokens revoked --worker-url=http://localhost:9091

# Override forge-api address
./bin/forge create --owner alice --dry-run --forge-url http://localhost:9090
```

## Demo Moments

### Demo 1 — Crash Recovery (saga-conductor)

Demonstrates saga-conductor's **at-least-once delivery** and env-forge's **`IsAlreadyDone` idempotency**.

```bash
# Start a dry-run provisioning (minikube)
./bin/forge create --owner alice --dry-run &

# Kill forge-worker pod while rds step is sleeping (5s delay)
kubectl delete pod -l app=forge-worker

# saga-conductor retries; forge-worker restarts and resumes from where it left off
# The rds step calls IsAlreadyDone — sees env.RDSInstanceID already set — skips
kubectl rollout status deploy/forge-worker
./bin/forge list
# → status: "ready", all 8 steps COMPLETED
```

**What to observe:** The conductor's retry counter increments. forge-worker resumes in the middle of the saga without duplicating the RDS instance.

---

### Demo 2 — Compensation Cascade (saga-conductor)

Demonstrates saga-conductor's **automatic backward compensation** when a step fails.

```bash
./bin/forge create --owner alice --dry-run --fail-at-health

# Watch saga-conductor trigger compensations in reverse order:
# health FAILED → config compensated → identity compensated →
# s3 compensated → ec2 compensated → rds compensated → vpc compensated
./bin/forge list
# → status: "failed"
```

**What to observe:** Each compensation step runs in reverse. The saga-conductor guarantees every successfully completed step gets its compensation called, even across retries.

---

### Demo 3 — svid-exchange Policy Lifecycle

Demonstrates **svid-exchange's policy CRUD** tied to environment lifecycle.

```bash
# 1. Provision an environment — creates the exchange policy in Step 5
./bin/forge create --owner bob --dry-run

# 2. Inspect the policy created by the identity step
kubectl port-forward svc/svid-exchange 8080:8080 &
grpcurl -plaintext localhost:8082 admin.v1.PolicyAdmin/ListPolicies
# → policy-env-<id8>: allows spiffe://.../env-<id>/app → env-<id>/db-proxy

# 3. Trigger a compensation cascade (policy is deleted in Step 5 compensate)
./bin/forge create --owner bob2 --dry-run --fail-at-health
grpcurl -plaintext localhost:8082 admin.v1.PolicyAdmin/ListPolicies
# → policy for bob2's env is gone (compensation deleted it)
```

**What to observe:** svid-exchange policies are created on successful identity step and deleted on compensation — the full CRUD lifecycle through env-forge.

---

### Demo 4 — JWT Token Exchange (forge-worker → forge-api)

Demonstrates **zero-trust service-to-service communication** via SPIFFE SVIDs and svid-exchange.

```bash
# Requires SPIRE entries registered (bash scripts/register-spire-entries.sh)

# Watch forge-worker logs during provisioning
kubectl logs -f -l app=forge-worker &

./bin/forge create --owner carol --dry-run

# In the logs you'll see:
# INF JWT token exchange enabled svid_exchange_addr=svid-exchange...:8080
# Each step call: forge-worker fetches JWT → adds Authorization: Bearer header
# forge-api validates the JWT using JWKS before allowing the state update

# Verify the SPIRE entries and current SVIDs:
kubectl exec -n spire statefulset/spire-server -- \
  /opt/spire/bin/spire-server entry show
```

**What to observe:** forge-worker presents its SPIFFE SVID to svid-exchange and receives a scoped ES256 JWT. forge-api verifies the JWT via the JWKS endpoint before writing any env state. Without a valid JWT (or when SPIRE entries are missing), forge-worker falls back to dev mode.

---

### Demo 5 — Dead-lettering (saga-conductor)

Demonstrates saga-conductor's **exhausted-retry dead-letter** state.

```bash
# 1. Break forge-worker so compensation always fails
kubectl set env deploy/forge-worker FORGE_API_URL=http://broken.invalid:9090

# 2. Trigger a compensation cascade
./bin/forge create --owner charlie --dry-run --fail-at-health

# 3. After 3 retries conductor marks saga COMPENSATION_FAILED
kubectl port-forward svc/saga-conductor 8080:8080 &
grpcurl -plaintext -d '{"saga_id": "<id>"}' localhost:8080 saga.v1.SagaOrchestrator/GetSaga
# → status: COMPENSATION_FAILED

# Restore forge-worker
kubectl set env deploy/forge-worker FORGE_API_URL=http://forge-api.default.svc.cluster.local:9090
```

**What to observe:** saga-conductor moves through COMPENSATING → COMPENSATION_FAILED after exhausting MaxRetries. The dead-letter state is persistent across conductor restarts.

---

### Demo 6 — ListSagas + Step Detail (saga-conductor)

Demonstrates **saga-conductor's `ListSagas` RPC** with status filtering and pagination, plus **`StepExecution.error_detail`** and **`failed_step`** in `forge status`.

```bash
# Run two sagas concurrently
./bin/forge create --owner alice --dry-run &
./bin/forge create --owner bob   --dry-run &

# List all running sagas (saga-conductor ListSagas with status filter)
./bin/forge sagas list --status=RUNNING
# → SAGA ID  STATUS         FAILED STEP  CREATED
# → abc12345 SAGA_STATUS_RUNNING          2026-03-23T10:00:00Z

# Step-level detail (GetSaga + error_detail + failed_step)
./bin/forge status <alice-env-id>
# → Saga abc12345 — SAGA_STATUS_RUNNING
# →   STEP      STATUS                 STARTED   COMPLETED  ERROR
# →   vpc       STEP_STATUS_SUCCEEDED  10:00:01  10:00:03
# →   rds       STEP_STATUS_RUNNING    10:00:04

# Paginate (page_size + cursor)
./bin/forge sagas list --page-size=1
# → Next page: --cursor=<token>
./bin/forge sagas list --page-size=1 --cursor=<token>
```

**saga-conductor features demonstrated:** `ListSagas` (status filter, pagination), `GetSaga` (steps, failed_step, error_detail JSON parsing).

---

### Demo 7 — AbortSaga (saga-conductor)

Demonstrates **saga-conductor's `AbortSaga` RPC** — moves a saga to ABORTED without triggering compensation.

```bash
# Start a long-running saga
./bin/forge create --owner charlie --dry-run &
sleep 3  # let it reach rds step (5s dry-run delay)

# Abort via forge CLI (proxied through forge-api → saga-conductor AbortSaga)
./bin/forge sagas abort <charlie-env-id>
# → Saga abc12345 aborted — status: SAGA_STATUS_ABORTED

./bin/forge status <charlie-env-id>
# → status: SAGA_STATUS_ABORTED  (no compensation triggered)
```

**saga-conductor features demonstrated:** `AbortSaga`, ABORTED terminal state.

---

### Demo 8 — Metrics + SSE Dashboard (saga-conductor)

Demonstrates saga-conductor's **Prometheus `/metrics` endpoint** and **real-time SSE `/dashboard`**.

```bash
kubectl port-forward svc/saga-conductor 8081:8081 &

# Prometheus metrics (saga execution counters + step durations)
curl -s http://localhost:8081/metrics | grep saga_
# → saga_executions_total{status="completed"} 3
# → saga_step_duration_seconds_bucket{...}

# Live SSE dashboard — watch state transitions in real time
# Run this in one terminal while starting a saga in another
curl -N http://localhost:8081/dashboard
# → data: {"id":"abc12345","status":"RUNNING","steps":[...]}
# → data: {"id":"abc12345","status":"RUNNING","steps":[{"name":"vpc","status":"SUCCEEDED"},...]}
```

**saga-conductor features demonstrated:** `Observer` interface (powers SSE dashboard), Prometheus `Recorder` interface, `/metrics`, `/dashboard`.

---

### Demo 9 — Graceful Drain (saga-conductor)

Demonstrates saga-conductor's **in-flight saga preservation** during rolling restarts.

```bash
# Start a saga
./bin/forge create --owner dave --dry-run &

# Rolling restart conductor while saga is running (drain waits for in-flight)
kubectl rollout restart deployment/saga-conductor

# Conductor resumes the in-flight saga via Resume() on startup
kubectl rollout status deployment/saga-conductor
./bin/forge status <dave-env-id>
# → status: ready  (all 8 steps completed — saga was not lost)
```

**saga-conductor features demonstrated:** `Drain()` (stops accepting new sagas, waits for in-flight), `Resume()` (re-drives sagas from RUNNING/COMPENSATING after crash).

---

### Demo 10 — idempotency_key + saga_timeout (saga-conductor)

Demonstrates **`idempotency_key`** preventing duplicate sagas and **`saga_timeout_seconds`** as a safety deadline.

```bash
# forge-api sends idempotency_key="env-<uuid>" on every CreateSaga.
# If the CLI retries (network blip), saga-conductor returns the EXISTING saga.
./bin/forge create --owner eve --dry-run &

# The conductor log shows the idempotency key on CreateSaga:
kubectl logs -l app=saga-conductor | grep idempotency
# → INF saga created idempotency_key=env-<uuid> saga_id=abc12345

# Confirm per-step timeouts/retries are set (different per step):
kubectl logs -l app=saga-conductor | grep "timeout\|max_retries"
# → vpc: timeout=30s max_retries=2 backoff=500ms
# → rds: timeout=600s max_retries=3 backoff=2000ms
```

**saga-conductor features demonstrated:** `idempotency_key` in `CreateSagaRequest`, `saga_timeout_seconds=300`, per-step `timeout_seconds`/`max_retries`/`retry_backoff_ms`.

---

### Demo 11 — Policy Lifecycle + GetPolicy + ReloadPolicy (svid-exchange)

Demonstrates **svid-exchange's full policy admin API** used by the identity step.

```bash
# Provision an environment — identity step calls CreatePolicy, GetPolicy (verify), ReloadPolicy
./bin/forge create --owner frank --dry-run

# Watch identity step logs:
kubectl logs -l app=forge-worker | grep -A3 identity
# → INF created policy name=policy-env-<id8>
# → INF verified policy subject=...env/app target=...env/db-proxy scopes=read,write
# → INF policies reloaded active_count=3

# List all policies via forge-worker HTTP proxy (SPIFFE mTLS → svid-exchange admin gRPC)
kubectl port-forward svc/forge-worker 9091:9091 &
./bin/forge policies list --worker-url=http://localhost:9091
# → NAME                        SOURCE   SUBJECT              TARGET           SCOPES      MAX TTL
# → forge-worker-to-forge-api   yaml     ...sa/forge-worker   ...sa/forge-api  env:read... 3600s
# → policy-env-<id8>            dynamic  ...env/app           ...env/db-proxy  read,write  3600s

# grpc_reflection=true lets grpcurl discover services without proto files (direct to admin port)
kubectl port-forward svc/svid-exchange 8082:8082 &
grpcurl -plaintext localhost:8082 list
# → admin.v1.PolicyAdmin
grpcurl -plaintext localhost:8082 admin.v1.PolicyAdmin/ListPolicies

# Reload policies without restart (ReloadPolicy RPC)
./bin/forge policies reload --worker-url=http://localhost:9091
# → Policies reloaded successfully.
```

**svid-exchange features demonstrated:** `CreatePolicy`, `ListPolicies`, `GetPolicy` (via List+filter), `ReloadPolicy`, `grpc_reflection=true`.

---

### Demo 12 — Token Revocation (svid-exchange)

Demonstrates **svid-exchange's `RevokeToken` and `ListRevokedTokens`** RPCs with BoltDB persistence.

```bash
# Get a JWT token ID from forge-worker logs (logged after each Exchange call)
kubectl logs -l app=forge-worker | grep token_id
# → INF JWT exchanged token_id=abc-jti-xyz expires_at=1711234567

# Revoke the token via forge-worker proxy (persisted in BoltDB across restarts)
kubectl port-forward svc/forge-worker 9091:9091 &
./bin/forge tokens revoke abc-jti-xyz \
  --worker-url=http://localhost:9091 \
  --expires-at=1711234567

# Confirm it's in the revocation list
./bin/forge tokens revoked --worker-url=http://localhost:9091
# → TOKEN ID      EXPIRES AT
# → abc-jti-xyz   2026-03-23T11:00:00Z

# Any subsequent request using this token is rejected by svid-exchange
# (even if the token hasn't expired naturally)
```

**svid-exchange features demonstrated:** `RevokeToken`, `ListRevokedTokens`, BoltDB-persisted revocation.

---

### Demo 13 — Scope Enforcement (svid-exchange)

Demonstrates **`HasScope`/`HasAllScopes`** from the svid-exchange client library enforced in forge-api.

```bash
# forge-api requires env:read on GET /internal/envs/{id}
# forge-api requires env:read + env:write on PUT /internal/envs/{id}

# The forge-worker-to-forge-api policy grants ["env:read", "env:write"]
# so normal provisioning works. Observe the scope check logs:
kubectl logs -l app=forge-api | grep "authorized\|denied"
# → INF internal GET authorized sub=spiffe://.../forge-worker scope=env:read env:write
# → INF internal PUT authorized sub=spiffe://.../forge-worker scope=env:read env:write

# JWT claims are extracted from context via svidclient.ClaimsFromContext
# and scope-checked via svidclient.HasScope / HasAllScopes
```

**svid-exchange features demonstrated:** `NewMiddleware` (replaces custom auth), `ClaimsFromContext`, `HasScope`, `HasAllScopes`.

---

### Demo 14 — Key Rotation + Rate Limiting (svid-exchange)

Demonstrates **svid-exchange's `key_rotation_interval`** and **rate limiting** configuration.

```bash
# key_rotation_interval="1h" is set in server.yaml ConfigMap.
# After one hour, svid-exchange rotates its signing key automatically.
kubectl logs -l app=svid-exchange | grep -i rotat
# → INF signing key rotated new_key_id=k2 old_key_id=k1

# forge-api's JWKS auto-refresh (every 5 minutes) picks up the new key.
# forge-worker's background token refresh (at 80% TTL) re-fetches tokens
# signed with the new key. Zero downtime key rotation.

# Rate limiting: 50 RPS, burst 10 (set in server.yaml)
# Exceeding the limit returns RESOURCE_EXHAUSTED from svid-exchange.

# AUDIT_HMAC_KEY enables tamper-evident audit logging:
kubectl logs -l app=svid-exchange | grep audit
# → {"level":"info","event":"exchange","sub":"spiffe://...","hmac":"<sha256>"}
```

**svid-exchange features demonstrated:** `key_rotation_interval`, `rate_limit_rps`/`rate_limit_burst`, `AUDIT_HMAC_KEY` (tamper-evident audit log), `GRPCCredentials()` (available on exchange client for gRPC-native injection).

---

## Development

```bash
# Install Go toolchain (pinned to 1.26.1)
mise install

# Run all checks (build → vet → lint → test)
make verify

# Individual steps
make build    # compile forge-api, forge-worker, forge binaries
make vet      # go vet
make lint     # golangci-lint v2
make test     # race-detector tests + coverage
```

## Environment Variables

### forge-api

| Variable | Default | Description |
|----------|---------|-------------|
| `STEP_ADDR` | `:9090` | HTTP listen address |
| `DB_PATH` | `env-forge.db` | BoltDB file path |
| `CONDUCTOR_ADDR` | `saga-conductor.default.svc.cluster.local:8080` | saga-conductor gRPC address |
| `FORGE_WORKER_URL` | `http://forge-worker.default.svc.cluster.local:9091` | forge-worker base URL (used as saga selfURL) |
| `SVIDEXCHANGE_JWKS_URL` | `""` | svid-exchange JWKS URL for JWT validation (empty = dev mode) |
| `TRUST_DOMAIN` | `cluster.local` | SPIFFE trust domain |

### forge-worker

| Variable | Default | Description |
|----------|---------|-------------|
| `WORKER_ADDR` | `:9091` | HTTP listen address |
| `FORGE_API_URL` | `http://forge-api.default.svc.cluster.local:9090` | forge-api base URL |
| `SPIFFE_ENDPOINT_SOCKET` | `""` | SPIRE Workload API socket (empty = dev mode, no JWT) |
| `SVIDEXCHANGE_ADDR` | `""` | svid-exchange data-plane gRPC address (port 8080) |
| `SVIDEXCHANGE_ADMIN_ADDR` | `""` | svid-exchange admin gRPC address (port 8082) |
| `TRUST_DOMAIN` | `cluster.local` | SPIFFE trust domain |
| `LOCAL_ENV_DIR` | `/tmp/envfiles` | Directory for generated .env files |
| `AWS_REGION` | `""` | AWS region (unset = no AWS clients) |
