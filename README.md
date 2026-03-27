# env-forge

Demo platform for **[saga-conductor](https://github.com/ngaddam369/saga-conductor)** (distributed transactions) and **[svid-exchange](https://github.com/ngaddam369/svid-exchange)** (zero-trust token exchange). Provisions isolated AWS developer environments (VPC, RDS, EC2, S3) via fully reversible saga orchestration.

## Quick Demo

```bash
# Prerequisites: mise, Go 1.26.1
mise install && make build
cd ~/go_projects/saga-conductor && make build && cd ~/go_projects/env-forge

# Run the interactive demo (no Kubernetes needed — ~5 min)
./demo.sh --local

# Full demo including SPIRE + JWT token exchange (requires minikube)
./demo.sh --minikube

# All 14 demo moments back-to-back
./demo.sh --all
```

## Demo Script Options

| Flag | Description |
|------|-------------|
| `--local` | Demos 1,2,5,6,7,8,9,10 — saga-conductor features, local binaries only (default) |
| `--minikube` | Demos 3,4,11–14 — svid-exchange + JWT features, requires minikube + SPIRE |
| `--all` | All 14 demos — runs local phase first, then minikube phase |
| `--auto` | Non-interactive — no pause prompts, suitable for CI |
| `--demo N` | Run only demo N (1–14) |

## What the Demo Covers

**saga-conductor features** (local mode — no Kubernetes):
- Demo 1 — Crash recovery + `IsAlreadyDone` idempotency
- Demo 2 — Automatic backward compensation cascade
- Demo 5 — Dead-lettering (`COMPENSATION_FAILED`)
- Demo 6 — `ListSagas` with status filter + pagination, step-level detail
- Demo 7 — `AbortSaga` (no compensation)
- Demo 8 — Prometheus metrics + real-time SSE dashboard
- Demo 9 — Graceful drain (`Drain()`) + `Resume()` after restart
- Demo 10 — `idempotency_key` + `saga_timeout_seconds` + per-step timeouts

**svid-exchange features** (minikube mode — requires SPIRE):
- Demo 3 — `CreatePolicy` / `DeletePolicy` lifecycle tied to environment
- Demo 4 — SPIFFE SVID → ES256 JWT token exchange (forge-worker → forge-api)
- Demo 11 — `ListPolicies` + `ReloadPolicy` + gRPC reflection
- Demo 12 — `RevokeToken` + `ListRevokedTokens` (BoltDB-persisted)
- Demo 13 — `HasScope` / `HasAllScopes` enforcement in forge-api middleware
- Demo 14 — Key rotation + rate limiting + HMAC audit logging

## Documentation

- [Architecture](docs/architecture.md) — system diagram, service table, data flow
- [Local Quick Start](docs/local-quickstart.md) — run everything without Kubernetes
- [Minikube Setup](docs/minikube-setup.md) — full SPIRE + JWT token exchange setup
- [CLI Reference](docs/cli-reference.md) — all `forge` commands and flags
- [Demo Moments](docs/demo-moments.md) — detailed walkthrough of all 14 demos
- [Environment Variables](docs/environment-variables.md) — all env var tables
- [Development](docs/development.md) — make targets, project layout, adding steps
