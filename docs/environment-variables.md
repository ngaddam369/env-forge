# Environment Variables

## forge-api

| Variable | Default | Description |
|----------|---------|-------------|
| `STEP_ADDR` | `:9090` | HTTP listen address |
| `DB_PATH` | `env-forge.db` | BoltDB file path |
| `CONDUCTOR_ADDR` | `saga-conductor.default.svc.cluster.local:8080` | saga-conductor gRPC address |
| `FORGE_WORKER_URL` | `http://forge-worker.default.svc.cluster.local:9091` | forge-worker base URL (used as saga step/compensate selfURL) |
| `SVIDEXCHANGE_JWKS_URL` | `""` | svid-exchange JWKS URL for JWT validation; empty = dev mode (validation disabled) |
| `TRUST_DOMAIN` | `cluster.local` | SPIFFE trust domain |

**Dev mode:** When `SVIDEXCHANGE_JWKS_URL` is not set, JWT validation is disabled entirely. All `/internal/*` requests are accepted without a token.

## forge-worker

| Variable | Default | Description |
|----------|---------|-------------|
| `WORKER_ADDR` | `:9091` | HTTP listen address |
| `FORGE_API_URL` | `http://forge-api.default.svc.cluster.local:9090` | forge-api base URL (for state reads/writes) |
| `SPIFFE_ENDPOINT_SOCKET` | `""` | SPIRE Workload API socket path; empty = dev mode (no JWT) |
| `SVIDEXCHANGE_ADDR` | `""` | svid-exchange data-plane gRPC address (port 8080 in cluster) |
| `SVIDEXCHANGE_ADMIN_ADDR` | `""` | svid-exchange admin gRPC address (port 8082 in cluster) |
| `TRUST_DOMAIN` | `cluster.local` | SPIFFE trust domain |
| `LOCAL_ENV_DIR` | `/tmp/envfiles` | Directory for generated `.env` files |
| `AWS_REGION` | `""` | AWS region; unset = no AWS clients (dry-run only) |

**Dev mode:** When `SPIFFE_ENDPOINT_SOCKET` is not set, forge-worker calls forge-api without a JWT. forge-api must also be in dev mode (`SVIDEXCHANGE_JWKS_URL` unset) for this to work.

**Admin proxy:** When `SVIDEXCHANGE_ADMIN_ADDR` is set but `SPIFFE_ENDPOINT_SOCKET` is not, forge-worker connects to the admin gRPC using insecure credentials. This is useful for local development against a local svid-exchange instance.

## saga-conductor

| Variable | Default | Description |
|----------|---------|-------------|
| `GRPC_ADDR` | `:8080` | gRPC listen address |
| `HEALTH_ADDR` | `:8081` | Health/metrics HTTP listen address |
| `DB_PATH` | `saga-conductor.db` | BoltDB file path |
| `SHUTDOWN_DRAIN_SECONDS` | `5` | Seconds to wait during graceful drain before forcing shutdown; `0` = skip drain wait |
| `SHUTDOWN_SAGA_TIMEOUT_SECONDS` | `30` | Timeout for each in-flight saga to complete during drain |
| `GRPC_STOP_TIMEOUT_SECONDS` | `30` | Timeout for gRPC server graceful stop |

## svid-exchange

| Variable | Default | Description |
|----------|---------|-------------|
| `GRPC_ADDR` | `:8080` | gRPC data-plane listen address |
| `ADMIN_ADDR` | `:8082` | gRPC admin listen address |
| `HEALTH_ADDR` | `:8081` | Health/JWKS HTTP listen address (GET /health/live, GET /jwks) |
| `POLICY_FILE` | `config/policy.example.yaml` | Path to policy YAML file |
| `AUDIT_HMAC_KEY` | `""` | HMAC key for tamper-evident audit log; empty = no HMAC |

Policy-level settings (in YAML): `key_rotation_interval`, `rate_limit_rps`, `rate_limit_burst`.

## Local Demo Defaults

The `demo.sh` script starts services with these specific values for the local demos:

```bash
# saga-conductor
DB_PATH=/tmp/demo-saga.db
HEALTH_ADDR=:8081
GRPC_ADDR=:8080
SHUTDOWN_DRAIN_SECONDS=0
SHUTDOWN_SAGA_TIMEOUT_SECONDS=60

# forge-api
DB_PATH=/tmp/demo-forge.db
STEP_ADDR=:9090
CONDUCTOR_ADDR=localhost:8080
FORGE_WORKER_URL=http://localhost:9091
# SVIDEXCHANGE_JWKS_URL omitted → dev mode

# forge-worker
FORGE_API_URL=http://localhost:9090
WORKER_ADDR=:9091
# SPIFFE_ENDPOINT_SOCKET omitted → dev mode
```
