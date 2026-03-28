# Minikube Setup (full SPIRE + JWT token exchange)

This setup enables the svid-exchange demos (3, 4, 11–14) with real SPIFFE SVIDs and ES256 JWT token exchange.

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/docs/start/)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [mise](https://mise.jdx.dev/)
- Docker (for building images)

## Quick Setup

```bash
minikube start
make minikube-setup   # build images, deploy SPIRE + all services (~3 min)
./demo.sh --minikube
```

## Resetting State Between Demo Runs

```bash
make minikube-teardown   # restart all pods with fresh BoltDB state
./demo.sh --minikube
```

`make minikube-teardown` restarts pods in a specific order (see [Teardown Ordering](#teardown-ordering) below). It does **not** delete PVCs or re-deploy SPIRE — only pod restarts are needed between runs.

---

## What `make minikube-setup` Does

### 1. Build and load Docker images (`minikube-images`)

Builds `forge-api` and `forge-worker` from local source, then loads all 7 required images into minikube's internal Docker daemon. All manifests use `imagePullPolicy: Never`, so every image must be pre-loaded — minikube cannot pull from external registries.

The forge images are loaded via `docker save | minikube ssh -- docker load` rather than `minikube image load` because the latter does not reliably replace an existing tag in-place.

### 2. Create `spire` namespace and bundle ConfigMap

The `spire-bundle` ConfigMap must exist before the SPIRE server starts — the server publishes its CA bundle there, and the agent reads it for bootstrap trust. Created empty with `|| true` so re-runs are safe.

### 3. Deploy SPIRE server, then agent (in order)

The server must be Ready before the agent is deployed: the agent connects to the server's socket to attest. The agent runs as a DaemonSet with `hostPID: true` for k8s_psat attestation.

### 4. Register SPIRE workload entries

Runs `scripts/register-spire-entries.sh`, which shells into the spire-server pod and creates entries for `svid-exchange`, `forge-api`, and `forge-worker`. Each entry maps a SPIFFE ID to a Kubernetes service account selector. On re-runs, "already exists" errors are non-fatal — existing entries remain valid.

### 5. Deploy saga-conductor, then svid-exchange

Both use `strategy: Recreate` (required for RWO PVCs — see [K8s Manifest Notes](#k8s-manifest-notes)).

### 6. Deploy forge-api and forge-worker

forge-api fetches JWKS from svid-exchange at startup (up to 10 retries at 3s). Since svid-exchange is already Ready by this point, the first fetch succeeds and JWT validation is enabled immediately.

---

## What `make minikube-teardown` Does

### Teardown Ordering

The teardown restarts pods in this specific order:

1. **svid-exchange first**
2. **forge-api second**
3. **forge-worker + saga-conductor last** (in parallel)

This order is critical. svid-exchange uses an **ephemeral** in-memory ECDSA P-256 signing key — generated fresh on every startup via `ecdsa.GenerateKey(elliptic.P256(), rand.Reader)`, never persisted. After svid-exchange restarts, the JWKS endpoint serves a new public key.

forge-api fetches and caches JWKS at startup. If forge-api were restarted before svid-exchange, it would cache the old key. After svid-exchange restarts with a new key, every JWT presented by forge-worker would fail signature verification (HTTP 401) until forge-api's 5-minute JWKS auto-refresh fired.

By restarting svid-exchange first and waiting for its `readinessProbe` to pass, forge-api is guaranteed to fetch the correct key when it starts.

### What gets reset

| Component | What resets |
|-----------|-------------|
| svid-exchange | New signing key, empty dynamic policy BoltDB, empty revocation BoltDB |
| forge-api | JWKS cache refreshed from new svid-exchange key; BoltDB **not** cleared (PVC persists) |
| forge-worker | Stateless between calls; SPIRE SVID re-issued on next request |
| saga-conductor | BoltDB cleared — `forge sagas list` returns empty |

> **Note:** The forge-api PVC (environment BoltDB) is **not** cleared. Previous demo environments accumulate in `forge list`. The demo script handles this correctly: demos that start provisioning in the background use `wait $pid` to wait for their specific process rather than polling `forge list` by owner name.

---

## K8s Manifest Notes

### `strategy: Recreate`

`saga-conductor`, `forge-api`, and `svid-exchange` all use ReadWriteOnce PVCs. RWO PVCs can only be mounted by one pod at a time. The default `RollingUpdate` strategy starts the new pod before terminating the old one — the new pod cannot open the BoltDB (lock held by the old pod) and enters `CrashLoopBackOff`, causing the rollout to time out.

`strategy: Recreate` terminates the old pod first, then starts the new one. This is the correct strategy for any single-replica deployment with a RWO PVC.

### svid-exchange `readinessProbe`

svid-exchange has a `readinessProbe` on `/health/live` (port 8081, `initialDelaySeconds: 5`). Without it, `kubectl rollout status` returns as soon as the container process starts — before the HTTP server is accepting connections. The readiness probe ensures the JWKS endpoint is live before teardown proceeds to restart forge-api.

---

## Manual Steps (reference)

If you need to set up without `make minikube-setup`:

```bash
# 1. Start minikube
minikube start

# 2. Load pre-built images
minikube image load ghcr.io/spiffe/spire-server:1.9.6
minikube image load ghcr.io/spiffe/spire-agent:1.9.6
minikube image load cgr.dev/chainguard/kubectl:latest
minikube image load ghcr.io/ngaddam369/saga-conductor:latest
minikube image load ghcr.io/ngaddam369/svid-exchange:latest

# 3. Build and load forge images
docker build -t ghcr.io/ngaddam369/env-forge-api:latest .
docker build -f Dockerfile.worker -t ghcr.io/ngaddam369/env-forge-worker:latest .
docker save ghcr.io/ngaddam369/env-forge-api:latest   | minikube ssh --native-ssh=false -- docker load
docker save ghcr.io/ngaddam369/env-forge-worker:latest | minikube ssh --native-ssh=false -- docker load

# 4. Deploy SPIRE
kubectl create namespace spire 2>/dev/null || true
kubectl create configmap spire-bundle -n spire 2>/dev/null || true
kubectl apply -f k8s/spire/spire-server.yaml
kubectl wait --for=condition=Ready pod -n spire -l app=spire-server --timeout=120s
kubectl apply -f k8s/spire/spire-agent.yaml
kubectl wait --for=condition=Ready pod -n spire -l app=spire-agent --timeout=120s
bash scripts/register-spire-entries.sh

# 5. Deploy services
kubectl apply -f k8s/saga-conductor.yaml
kubectl wait --for=condition=Ready pod -l app=saga-conductor --timeout=120s
kubectl apply -f k8s/svid-exchange.yaml
kubectl wait --for=condition=Ready pod -l app=svid-exchange --timeout=120s
kubectl apply -f k8s/forge-api.yaml
kubectl apply -f k8s/forge-worker.yaml
kubectl wait --for=condition=Ready pod -l app=forge-api --timeout=120s
kubectl wait --for=condition=Ready pod -l app=forge-worker --timeout=120s
```

## (Optional) AWS Credentials

For real provisioning (non-dry-run):

```bash
kubectl create secret generic aws-creds \
  --from-literal=AWS_ACCESS_KEY_ID=<key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret> \
  --from-literal=AWS_REGION=us-east-1
```

## Verify

```bash
kubectl port-forward svc/forge-api 9090:9090 &
./bin/forge create --owner alice --dry-run
# → Environment <id> — status: ready  (all 8 steps + JWT token exchange)

kubectl logs -l app=forge-worker | grep "JWT token exchange"
# → JWT token exchange enabled
```

## Troubleshooting

**JWT validation fails (401) after re-running demos**: Run `make minikube-teardown` to restart pods in the correct order. forge-api must start after svid-exchange so it picks up the new ephemeral signing key.

**SPIRE agent stuck in Pending**: Check that `hostPID: true` is set in the agent DaemonSet and that the SPIRE socket directory is available on the node.

**forge-worker logs show "dev mode"**: SPIRE workload entries may not be registered. Re-run `bash scripts/register-spire-entries.sh`.

**Image pull errors**: All images must be pre-loaded — manifests set `imagePullPolicy: Never`. Re-run `make minikube-images` to rebuild and reload forge images.

**Rollout hangs on saga-conductor or forge-api**: These deployments use RWO PVCs. If you changed the rollout strategy from `Recreate` back to `RollingUpdate`, the new pod will deadlock on the BoltDB lock. Restore `strategy: Recreate` in the manifest and re-apply.
