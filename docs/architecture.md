# Architecture

## System Diagram

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

## Data Flow

1. **CLI → forge-api**: `forge create` POSTs to `/envs/create`; forge-api creates the saga in saga-conductor and polls the env status.
2. **saga-conductor → forge-worker**: For each step, conductor calls `POST /steps/{name}` and `POST /steps/{name}/compensate`.
3. **forge-worker → svid-exchange** (minikube only): Each step's state read/write calls first exchanges the worker's SPIFFE SVID for a scoped ES256 JWT.
4. **forge-worker → forge-api**: Internal HTTP calls (`GET|PUT /internal/envs/{id}`) with `Authorization: Bearer <JWT>`.
5. **forge-api JWT validation**: forge-api verifies the JWT against svid-exchange's JWKS endpoint. Scope enforcement gates the internal routes.

## Idempotency

Each saga step implements `IsAlreadyDone() bool`. If forge-worker crashes mid-saga, saga-conductor retries the current step. The step checks whether its resource ID is already set in BoltDB and skips the actual work if so. This guarantees exactly-once effect despite at-least-once delivery.

## BoltDB Ownership

forge-api holds an exclusive write lock on the BoltDB file. The `forge list` and `forge status` CLI commands use `OpenReadOnly()` which opens a second read-only handle — safe to run concurrently with forge-api.
