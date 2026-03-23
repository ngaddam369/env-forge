# env-forge

Arc 1 demo platform. Provisions isolated AWS developer environments (VPC, RDS, EC2, S3) via a saga orchestration pattern. Every provisioning step is reversible — failed sagas trigger automatic compensation back to a clean state.

Runs entirely on **minikube** for local demos.

## Architecture

```
forge create --owner alice
    │
    ▼
saga-conductor (orchestrator)
    │  POST /steps/{name}          POST /steps/{name}/compensate
    ▼
env-forge (step worker, port 9090)
    │
    ├── Step 1: vpc        — VPC + subnets + security groups
    ├── Step 2: rds        — RDS postgres db.t3.micro
    ├── Step 3: ec2        — t3.micro with SPIRE agent
    ├── Step 4: s3         — versioned S3 bucket
    ├── Step 5: identity   — svid-exchange policy
    ├── Step 6: config     — config.json → S3 + local .env
    ├── Step 7: health     — SELECT 1, SPIRE health check
    └── Step 8: registry   — mark environment "ready"
```

All environment state is persisted in BoltDB so crash recovery is automatic — each step checks `IsAlreadyDone` before acting.

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/docs/start/)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [mise](https://mise.jdx.dev/) (installs Go 1.26.1 automatically)
- [gh](https://cli.github.com/) (for GitHub workflows)

## Setup

```bash
# Install Go via mise
mise install

# Start minikube
minikube start

# Deploy SPIRE (server + agent)
kubectl apply -f k8s/spire/spire-server.yaml
kubectl apply -f k8s/spire/spire-agent.yaml

# Deploy saga-conductor and svid-exchange
kubectl apply -f k8s/saga-conductor.yaml
kubectl apply -f k8s/svid-exchange.yaml

# (Optional) Create AWS credentials secret for real provisioning
kubectl create secret generic aws-creds \
  --from-literal=AWS_ACCESS_KEY_ID=<key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret> \
  --from-literal=AWS_REGION=us-east-1

# Deploy env-forge
kubectl apply -f k8s/env-forge.yaml

# Wait for all pods to be ready
kubectl wait --for=condition=Ready pod -l app=env-forge --timeout=120s
kubectl wait --for=condition=Ready pod -l app=saga-conductor --timeout=120s
kubectl wait --for=condition=Ready pod -l app=svid-exchange --timeout=120s
```

## CLI Usage

```bash
# Build the CLI
make build

# Dry-run: provision without real AWS calls
./bin/forge create --owner alice --dry-run

# Real provisioning (requires AWS credentials)
./bin/forge create --owner alice

# Check status
./bin/forge status <env-id>

# List all environments
./bin/forge list

# Destroy an environment (triggers saga compensation)
./bin/forge destroy <env-id>
```

## Demo Moments

### Demo 1 — Crash recovery

Kill env-forge mid-flight during the RDS polling step. The conductor retries and env-forge resumes without creating duplicate resources (`IsAlreadyDone` calls `DescribeDBInstances`).

```bash
# Start a dry-run provisioning
./bin/forge create --owner alice --dry-run &

# Kill the pod while rds step is running
kubectl delete pod -l app=env-forge

# Watch conductor retry and env-forge resume from where it left off
kubectl rollout status deploy/env-forge
./bin/forge status <env-id>
```

### Demo 2 — Compensation cascade

Force a health validation failure to trigger automatic rollback of all completed steps.

```bash
./bin/forge create --owner alice --dry-run --fail-at-health

# Watch each compensation step run in reverse order
./bin/forge status <env-id>
# env-forge → identity compensated → s3 compensated → ec2 compensated → rds compensated → vpc compensated
```

### Demo 3 — Dead-lettering

Break the RDS compensation endpoint so the conductor exhausts its retries and dead-letters the saga.

```bash
# Edit k8s/env-forge.yaml: point rds compensate_url to a non-existent endpoint
# Then trigger a compensation cascade (--fail-at-health)
./bin/forge create --owner alice --dry-run --fail-at-health

# After 3 retries the saga moves to COMPENSATION_FAILED
./bin/forge status <env-id>
```

## Development

```bash
# Run all checks
make verify

# Individual steps
make build    # compile
make vet      # go vet
make lint     # golangci-lint
make test     # race-detector tests + coverage
```

## Environment Variables (serve mode)

| Variable            | Default                                                  | Description                         |
|---------------------|----------------------------------------------------------|-------------------------------------|
| `STEP_ADDR`         | `:9090`                                                  | HTTP listen address                 |
| `DB_PATH`           | `env-forge.db`                                           | BoltDB file path                    |
| `CONDUCTOR_ADDR`    | `saga-conductor.default.svc.cluster.local:8080`          | saga-conductor gRPC address         |
| `SELF_URL`          | `http://env-forge.default.svc.cluster.local:9090`        | Base URL conductor uses to call us  |
| `SVIDEXCHANGE_ADDR` | `svid-exchange.default.svc.cluster.local:8080`           | svid-exchange gRPC address          |
| `TRUST_DOMAIN`      | `cluster.local`                                          | SPIFFE trust domain                 |
| `LOCAL_ENV_DIR`     | `/tmp/envfiles`                                          | Directory for generated .env files  |
| `AWS_REGION`        | `us-east-1`                                              | AWS region                          |
