# Local Quick Start (no Kubernetes)

Run all services as local binaries for the fastest dev loop. In this mode forge-worker calls forge-api without JWT (`SPIFFE_ENDPOINT_SOCKET` unset — dev mode).

## Prerequisites

- [mise](https://mise.jdx.dev/) — installs Go 1.26.1 automatically
- [saga-conductor](https://github.com/ngaddam369/saga-conductor) checked out at `~/go_projects/saga-conductor`

## Build

```bash
# env-forge
cd ~/go_projects/env-forge
mise install && make build

# saga-conductor
cd ~/go_projects/saga-conductor
mise install && make build
```

## Start Services

Open four terminal windows.

**Terminal A — saga-conductor**
```bash
cd ~/go_projects/saga-conductor
DB_PATH=/tmp/saga.db \
  HEALTH_ADDR=:8081 \
  GRPC_ADDR=:8080 \
  ./bin/saga-conductor
```

**Terminal B — forge-api** (JWT validation disabled — no `SVIDEXCHANGE_JWKS_URL`)
```bash
cd ~/go_projects/env-forge
DB_PATH=/tmp/forge.db \
  STEP_ADDR=:9090 \
  CONDUCTOR_ADDR=localhost:8080 \
  FORGE_WORKER_URL=http://localhost:9091 \
  ./bin/forge-api
```

**Terminal C — forge-worker** (dev mode — no SPIFFE socket)
```bash
cd ~/go_projects/env-forge
FORGE_API_URL=http://localhost:9090 \
  WORKER_ADDR=:9091 \
  ./bin/forge-worker
```

**Terminal D — CLI**
```bash
cd ~/go_projects/env-forge

# Dry-run: no AWS credentials needed (~21s)
./bin/forge create --owner alice --dry-run

# Check status
./bin/forge list
./bin/forge status <env-id-prefix>
```

## What Happens During Dry-Run

| Step | Duration | What happens |
|------|----------|-------------|
| vpc | ~2s | Sleeps, writes fake VPCID to BoltDB |
| rds | ~8s | Sleeps (5s + 3s), writes fake RDSInstanceID |
| ec2 | ~3s | Sleeps, writes fake EC2InstanceID |
| s3 | ~1s | Sleeps, writes fake S3Bucket |
| identity | ~1s | Sleeps (no svid-exchange in local mode) |
| config | ~0.5s | Writes fake config.json |
| health | ~1s | Sleeps (SELECT 1 skipped in dry-run) |
| registry | ~0.2s | Marks env StatusReady in BoltDB |

Total: ~18–21s, all 8 steps COMPLETED.

## Automated Demo

The `demo.sh` script automates all local demos (1,2,5,6,7,8,9,10) including service lifecycle:

```bash
./demo.sh --local       # runs saga-conductor feature demos
./demo.sh --local --auto  # non-interactive (CI-friendly)
./demo.sh --demo 2      # run only Demo 2 (compensation cascade)
```

See the [demo script documentation](../README.md#quick-demo) for full options.
