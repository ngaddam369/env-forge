# env-forge

Demo platform. Provisions isolated AWS developer environments (VPC, RDS, EC2, S3) via saga orchestration — every step is reversible. Demonstrates saga-conductor (distributed transactions) and svid-exchange (zero-trust token exchange) working together in a Kubernetes-native deployment.

## Architecture

```
forge create --owner alice --dry-run
    │
    │  POST /envs/create
    ▼
forge serve (HTTP step server, port 9090)
    │  gRPC
    ▼
saga-conductor (orchestrator)
    │  POST /steps/{name}          POST /steps/{name}/compensate
    ▼
forge serve (steps handler)
    │
    ├── Step 1: vpc        — VPC + subnets + security groups
    ├── Step 2: rds        — RDS postgres db.t3.micro
    ├── Step 3: ec2        — t3.micro with SPIRE agent
    ├── Step 4: s3         — versioned S3 bucket
    ├── Step 5: identity   — svid-exchange exchange policy
    ├── Step 6: config     — config.json → S3 + local .env
    ├── Step 7: health     — SELECT 1, SPIRE health check
    └── Step 8: registry   — mark environment "ready" in BoltDB
```

Every step stores its output in BoltDB (keyed by env_id). Each step checks `IsAlreadyDone` before executing, so crash recovery after any failure is automatic.

## Services

| Service | Purpose | Port |
|---------|---------|------|
| **env-forge** | Step worker + CLI | 9090 (HTTP) |
| **saga-conductor** | Saga orchestrator | 8080 (gRPC) |
| **svid-exchange** | Token exchange | 8080 (gRPC) |
| **SPIRE server** | Workload identity CA | 8081 (gRPC) |
| **SPIRE agent** | Workload attestation | DaemonSet |

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/docs/start/)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [skaffold](https://skaffold.dev/docs/install/) or `kubectl port-forward` for local CLI access
- [mise](https://mise.jdx.dev/) (installs Go 1.26.1 automatically)

## Minikube Setup

```bash
# Start minikube
minikube start

# Deploy SPIRE (server then agent — agent waits for server to be ready)
kubectl apply -f k8s/spire/spire-server.yaml
kubectl apply -f k8s/spire/spire-agent.yaml

# Deploy saga-conductor and svid-exchange
kubectl apply -f k8s/saga-conductor.yaml
kubectl apply -f k8s/svid-exchange.yaml

# Wait for dependencies
kubectl wait --for=condition=Ready pod -l app=saga-conductor --timeout=120s
kubectl wait --for=condition=Ready pod -l app=svid-exchange --timeout=120s

# (Optional) Create AWS credentials secret for real provisioning
kubectl create secret generic aws-creds \
  --from-literal=AWS_ACCESS_KEY_ID=<key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret> \
  --from-literal=AWS_REGION=us-east-1

# Deploy env-forge
kubectl apply -f k8s/env-forge.yaml
kubectl wait --for=condition=Ready pod -l app=env-forge --timeout=120s
```

## CLI Usage (via kubectl port-forward)

```bash
# Build the CLI
mise install && make build

# Forward forge serve port for CLI access
kubectl port-forward svc/env-forge 9090:9090 &

# Provision an environment (dry-run, no AWS needed)
./bin/forge create --owner alice --dry-run

# Check status
curl -s http://localhost:9090/envs | python3 -m json.tool

# List environments
./bin/forge list --db $(kubectl exec deploy/env-forge -- printenv DB_PATH)
```

For a simpler local demo without minikube, see the **Local Quick Start** section below.

## Local Quick Start (no Kubernetes)

Run all services as local binaries for the fastest dev loop:

```bash
# Build
mise install && make build
cd ~/go_projects/saga-conductor && make build
cd ~/go_projects/svid-exchange && make build

# Terminal A: saga-conductor
cd ~/go_projects/saga-conductor
DB_PATH=/tmp/saga.db ./bin/saga-conductor

# Terminal B: forge serve (connects to conductor at localhost:8080)
cd ~/go_projects/env-forge
./bin/forge serve --db /tmp/forge.db

# Terminal C: provision
./bin/forge create --owner alice --dry-run
# ✓ Environment ready in ~20s, all 8 steps completed
```

## Demo Moments

### Demo 1 — Crash Recovery (saga-conductor)

Demonstrates saga-conductor's **at-least-once delivery** and env-forge's **`IsAlreadyDone` idempotency**.

```bash
# Start a dry-run provisioning (minikube)
./bin/forge create --owner alice --dry-run &

# Kill env-forge pod while rds step is sleeping (5s delay)
kubectl delete pod -l app=env-forge

# saga-conductor retries; env-forge restarts and resumes from where it left off
# The rds step calls DescribeDBInstances — sees "available" — skips recreation
kubectl rollout status deploy/env-forge
curl -s http://localhost:9090/envs | python3 -m json.tool
# → status: "ready", all 8 steps COMPLETED
```

**What to observe:** The conductor's retry counter increments. env-forge resumes
in the middle of the saga without duplicating the RDS instance.

---

### Demo 2 — Compensation Cascade (saga-conductor)

Demonstrates saga-conductor's **automatic backward compensation** when a step fails.

```bash
./bin/forge create --owner alice --dry-run --fail-at-health

# Watch saga-conductor trigger compensations in reverse order:
# health FAILED → config compensated → identity compensated →
# s3 compensated → ec2 compensated → rds compensated → vpc compensated
curl -s http://localhost:9090/envs | python3 -m json.tool
# → status: "failed" (set by provisioner goroutine after saga completes)
```

**What to observe:** Each compensation step runs in reverse. The saga-conductor
guarantees every successfully completed step gets its compensation called, even
across retries.

---

### Demo 3 — svid-exchange Policy Lifecycle

Demonstrates **svid-exchange's policy CRUD** tied to environment lifecycle.

```bash
# 1. Provision an environment — creates the exchange policy in Step 5
./bin/forge create --owner bob --dry-run

# 2. Inspect the policy created by env-forge's identity step
kubectl port-forward svc/svid-exchange 8080:8080 &
# Use grpcurl or the svid-exchange admin API to list policies:
grpcurl -plaintext localhost:8080 admin.v1.PolicyAdmin/ListPolicies
# → policy-env-<id8>: allows spiffe://.../env-<id>/app → env-<id>/db-proxy

# 3. Trigger a compensation cascade (policy is deleted in Step 5 compensate)
./bin/forge create --owner bob2 --dry-run --fail-at-health
grpcurl -plaintext localhost:8080 admin.v1.PolicyAdmin/ListPolicies
# → policy for bob2's env is gone (compensation deleted it)
```

**What to observe:** svid-exchange policies are created on successful identity
step and deleted on compensation — the full CRUD lifecycle through env-forge.

---

### Demo 4 — Dead-lettering (saga-conductor)

Demonstrates saga-conductor's **exhausted-retry dead-letter** state.

```bash
# 1. Deploy a broken env-forge that always fails the rds compensate endpoint
kubectl set env deploy/env-forge SELF_URL=http://broken.invalid:9090

# 2. Trigger a compensation cascade
./bin/forge create --owner charlie --dry-run --fail-at-health

# 3. After 3 retries conductor marks saga COMPENSATION_FAILED
# Check conductor directly:
kubectl port-forward svc/saga-conductor 8080:8080 &
grpcurl -plaintext -d '{"saga_id": "<id>"}' localhost:8080 saga.v1.SagaOrchestrator/GetSaga
# → status: COMPENSATION_FAILED

# Restore env-forge
kubectl set env deploy/env-forge SELF_URL=http://env-forge.default.svc.cluster.local:9090
```

**What to observe:** saga-conductor moves through COMPENSATING → COMPENSATION_FAILED
after exhausting MaxRetries. The dead-letter state is persistent across conductor restarts.

---

## Development

```bash
# Install Go toolchain (pinned to 1.26.1)
mise install

# Run all checks (build → vet → lint → test)
make verify

# Individual steps
make build    # compile forge binary
make vet      # go vet
make lint     # golangci-lint v2
make test     # race-detector tests + coverage
```

## Environment Variables (forge serve)

| Variable            | Default                                                  | Description                         |
|---------------------|----------------------------------------------------------|-------------------------------------|
| `STEP_ADDR`         | `:9090`                                                  | HTTP listen address                 |
| `DB_PATH`           | `env-forge.db`                                           | BoltDB file path                    |
| `CONDUCTOR_ADDR`    | `saga-conductor.default.svc.cluster.local:8080`          | saga-conductor gRPC address         |
| `SELF_URL`          | `http://env-forge.default.svc.cluster.local:9090`        | Base URL conductor uses to call us  |
| `SVIDEXCHANGE_ADDR` | `svid-exchange.default.svc.cluster.local:8080`           | svid-exchange gRPC address          |
| `TRUST_DOMAIN`      | `cluster.local`                                          | SPIFFE trust domain                 |
| `LOCAL_ENV_DIR`     | `/tmp/envfiles`                                          | Directory for generated .env files  |
| `AWS_REGION`        | `us-east-1`                                              | AWS region (unset = no AWS clients) |
