# Minikube Setup (full SPIRE + JWT token exchange)

This setup enables the svid-exchange demos (3, 4, 11–14) with real SPIFFE SVIDs and ES256 JWT token exchange.

## Prerequisites

- [minikube](https://minikube.sigs.k8s.io/docs/start/)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [mise](https://mise.jdx.dev/)
- Docker (for building images)

## 1. Start Minikube

```bash
minikube start
```

## 2. Load Docker Images

All manifests use `imagePullPolicy: Never` — images must be pre-loaded into minikube. Minikube with the Docker driver cannot pull from external registries in offline or restricted environments.

```bash
# Pre-built images
minikube image load ghcr.io/spiffe/spire-server:1.9.6
minikube image load ghcr.io/spiffe/spire-agent:1.9.6
minikube image load cgr.dev/chainguard/kubectl:latest
minikube image load ghcr.io/ngaddam369/saga-conductor:latest
minikube image load ghcr.io/ngaddam369/svid-exchange:latest

# Build and load forge images
cd ~/go_projects/env-forge
docker build -t ghcr.io/ngaddam369/env-forge-api:latest .
docker build -f Dockerfile.worker -t ghcr.io/ngaddam369/env-forge-worker:latest .
minikube image load ghcr.io/ngaddam369/env-forge-api:latest
minikube image load ghcr.io/ngaddam369/env-forge-worker:latest
```

## 3. Deploy SPIRE

The spire-bundle ConfigMap must exist before the server starts — it's where the server publishes its CA bundle and where the agent reads its bootstrap trust. Always create it empty first.

```bash
# Create the bundle ConfigMap before deploying the server
kubectl create configmap spire-bundle -n spire 2>/dev/null || true

# Deploy SPIRE server
kubectl apply -f k8s/spire/spire-server.yaml
kubectl wait --for=condition=Ready pod -n spire -l app=spire-server --timeout=120s

# Deploy SPIRE agent
kubectl apply -f k8s/spire/spire-agent.yaml
kubectl wait --for=condition=Ready pod -n spire -l app=spire-agent --timeout=120s

# Register SPIRE workload entries (svid-exchange, forge-api, forge-worker)
bash scripts/register-spire-entries.sh
```

## 4. Deploy Supporting Services

```bash
# saga-conductor
kubectl apply -f k8s/saga-conductor.yaml
kubectl wait --for=condition=Ready pod -l app=saga-conductor --timeout=120s

# svid-exchange
kubectl apply -f k8s/svid-exchange.yaml
kubectl wait --for=condition=Ready pod -l app=svid-exchange --timeout=120s
```

## 5. Deploy env-forge

```bash
kubectl apply -f k8s/forge-api.yaml
kubectl apply -f k8s/forge-worker.yaml
kubectl wait --for=condition=Ready pod -l app=forge-api --timeout=120s
kubectl wait --for=condition=Ready pod -l app=forge-worker --timeout=120s
```

## 6. (Optional) AWS Credentials

For real provisioning (non-dry-run), create an AWS credentials secret:

```bash
kubectl create secret generic aws-creds \
  --from-literal=AWS_ACCESS_KEY_ID=<key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret> \
  --from-literal=AWS_REGION=us-east-1
```

## 7. Verify

```bash
# Port-forward forge-api and test
kubectl port-forward svc/forge-api 9090:9090 &

cd ~/go_projects/env-forge
./bin/forge create --owner alice --dry-run
# → Environment <id> — status: ready  (all 8 steps + JWT token exchange)
```

You should see `JWT token exchange enabled` in the forge-worker logs:
```bash
kubectl logs -l app=forge-worker | grep "JWT token exchange"
```

## 8. Run Minikube Demos

```bash
# Run all minikube demos (3, 4, 11–14) with automatic port-forwards
./demo.sh --minikube

# Non-interactive mode
./demo.sh --minikube --auto

# Specific demo
./demo.sh --demo 11
```

## Troubleshooting

**SPIRE agent stuck in Pending**: Check that `hostPID: true` is set in the agent DaemonSet and that the node has the SPIRE socket directory available.

**forge-worker logs show "dev mode"**: The SPIRE workload entries may not be registered. Re-run `bash scripts/register-spire-entries.sh`.

**JWT validation fails on forge-api**: Verify `SVIDEXCHANGE_JWKS_URL` in the forge-api ConfigMap matches the actual svid-exchange health port (8081 internally, not the admin port 8082).

**Image pull errors**: Make sure all images are loaded via `minikube image load` — the manifests set `imagePullPolicy: Never` explicitly.
