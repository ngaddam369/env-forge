MODULE      := github.com/ngaddam369/env-forge
GOTOOLCHAIN := go1.26.1

export GOTOOLCHAIN

## build: compile all binaries (forge-api, forge-worker, forge CLI)
build:
	go build -o bin/forge-api    ./cmd/forge-api
	go build -o bin/forge-worker ./cmd/forge-worker
	go build -o bin/forge        ./cmd/forge

## fmt: format all Go source files in place
fmt:
	gofmt -w .

## vet: run go vet
vet:
	go vet ./...

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## test: run all tests with race detector and show coverage summary
test:
	go test -v -race -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | grep -E "^total|^github"

## verify: run the full checklist (fmt → build → vet → lint → test)
verify: fmt build vet lint test
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf bin/ coverage.out

## tidy: tidy and verify go modules
tidy:
	go mod tidy
	go mod verify

## minikube-images: build forge Docker images and load all required images into minikube
minikube-images:
	@echo "==> Building forge Docker images..."
	docker build -t ghcr.io/ngaddam369/env-forge-api:latest .
	docker build -f Dockerfile.worker -t ghcr.io/ngaddam369/env-forge-worker:latest .
	@echo "==> Loading pre-built images into minikube..."
	minikube image load ghcr.io/spiffe/spire-server:1.9.6
	minikube image load ghcr.io/spiffe/spire-agent:1.9.6
	minikube image load cgr.dev/chainguard/kubectl:latest
	minikube image load ghcr.io/ngaddam369/saga-conductor:latest
	minikube image load ghcr.io/ngaddam369/svid-exchange:latest
	@echo "==> Loading forge images into minikube..."
	docker save ghcr.io/ngaddam369/env-forge-api:latest   | minikube ssh --native-ssh=false -- docker load
	docker save ghcr.io/ngaddam369/env-forge-worker:latest | minikube ssh --native-ssh=false -- docker load
	@echo "==> Images loaded."

## minikube-setup: full minikube setup — deploy SPIRE, supporting services, and env-forge
minikube-setup: minikube-images
	@echo "==> Creating spire namespace and bundle ConfigMap..."
	kubectl create namespace spire 2>/dev/null || true
	kubectl create configmap spire-bundle -n spire 2>/dev/null || true
	@echo "==> Deploying SPIRE server..."
	kubectl apply -f k8s/spire/spire-server.yaml
	kubectl wait --for=condition=Ready pod -n spire -l app=spire-server --timeout=120s
	@echo "==> Deploying SPIRE agent..."
	kubectl apply -f k8s/spire/spire-agent.yaml
	kubectl wait --for=condition=Ready pod -n spire -l app=spire-agent --timeout=120s
	@echo "==> Registering SPIRE workload entries..."
	bash scripts/register-spire-entries.sh
	@echo "==> Deploying saga-conductor..."
	kubectl apply -f k8s/saga-conductor.yaml
	kubectl wait --for=condition=Ready pod -l app=saga-conductor --timeout=120s
	@echo "==> Deploying svid-exchange..."
	kubectl apply -f k8s/svid-exchange.yaml
	kubectl wait --for=condition=Ready pod -l app=svid-exchange --timeout=120s
	@echo "==> Deploying forge-api and forge-worker..."
	kubectl apply -f k8s/forge-api.yaml
	kubectl apply -f k8s/forge-worker.yaml
	kubectl wait --for=condition=Ready pod -l app=forge-api --timeout=120s
	kubectl wait --for=condition=Ready pod -l app=forge-worker --timeout=120s
	@echo "==> Minikube setup complete. Run ./demo.sh --minikube to start the demo."

## minikube-teardown: restart all deployments to clear BoltDB state for a clean re-run
## svid-exchange is restarted FIRST because it generates an ephemeral signing key on every
## startup. forge-api fetches the JWKS on startup, so it must start AFTER svid-exchange is
## ready — otherwise forge-api caches a stale key and JWT validation fails (401).
minikube-teardown:
	@echo "==> Restarting svid-exchange (generates new ephemeral signing key)..."
	kubectl rollout restart deployment/svid-exchange
	kubectl rollout status  deployment/svid-exchange --timeout=180s
	@echo "==> Restarting forge-api (re-fetches JWKS from the new svid-exchange key)..."
	kubectl rollout restart deployment/forge-api
	kubectl rollout status  deployment/forge-api --timeout=120s
	@echo "==> Restarting forge-worker and saga-conductor..."
	kubectl rollout restart deployment/forge-worker deployment/saga-conductor
	kubectl rollout status  deployment/forge-worker --timeout=120s
	kubectl rollout status  deployment/saga-conductor --timeout=180s
	@echo "==> Teardown complete. All pods restarted with fresh state."

.PHONY: build fmt lint test verify clean tidy vet minikube-images minikube-setup minikube-teardown
