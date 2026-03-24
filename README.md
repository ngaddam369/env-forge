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

# Check status
./bin/forge status <env-id-prefix>

# List environments
./bin/forge list

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
