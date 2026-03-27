# Demo Moments

All 14 demo moments covered by `demo.sh`. Demos 1–2, 5–10 run locally (no Kubernetes). Demos 3–4, 11–14 require minikube + SPIRE.

Run with the demo script:
```bash
./demo.sh --local     # demos 1,2,5,6,7,8,9,10
./demo.sh --minikube  # demos 3,4,11,12,13,14
./demo.sh --demo N    # single demo
```

---

## Local Mode (saga-conductor features)

### Demo 1 — Crash Recovery

**Features:** `Resume()`, `IsAlreadyDone`, per-step idempotency, at-least-once delivery.

```bash
# Start provisioning in background
./bin/forge create --owner demo1 --dry-run &

# Kill forge-worker while rds step is mid-sleep (t=4s: vpc done at 2s, rds mid-sleep)
sleep 4
kill <forge-worker-pid>

# Restart forge-worker — saga-conductor retries; IsAlreadyDone skips completed vpc step
./bin/forge-worker &
wait_for_saga_state demo1 ready

./bin/forge list
./bin/forge status <demo1-env-prefix>
```

**Observe:** vpc step was skipped on retry (VPCID already in BoltDB — `IsAlreadyDone=true`). rds step re-ran from the beginning. All 8 steps complete.

**Timing detail:**
- vpc dry-run: 2s → VPCID written to BoltDB
- rds dry-run: 5s + 3s = 8s (writes only at end)
- Kill at 4s: vpc complete, rds mid-first-sleep
- Conductor retries rds ~2s after the kill

---

### Demo 2 — Compensation Cascade

**Features:** `COMPENSATING` saga state, automatic reverse rollback, all 7 compensation steps.

```bash
./bin/forge create --owner demo2 --dry-run --fail-at-health

./bin/forge list
# → status: failed

./bin/forge status <demo2-env-prefix>
# Shows: health FAILED → config COMPENSATED → identity COMPENSATED →
#        s3 COMPENSATED → ec2 COMPENSATED → rds COMPENSATED → vpc COMPENSATED
```

**Observe:** Seven compensation steps run in exact reverse order. The saga reaches `FAILED` (not `COMPENSATION_FAILED`). Each step's compensate endpoint is called once per compensation cycle.

---

### Demo 5 — Dead-lettering

**Features:** `COMPENSATION_FAILED` terminal state, exhausted-retry dead-letter, saga persistence across conductor restarts.

```bash
# Start provisioning in background
./bin/forge create --owner demo5 --dry-run --fail-at-health &

# Kill forge-worker after all forward steps complete but before compensation finishes
# (t=11s: vpc=2s + rds=8s = 10s + 1s buffer; health step will fail, triggering compensation)
sleep 11
kill <forge-worker-pid>

# Compensation calls from conductor now get connection refused
# After 3 retries at 2000ms base backoff → COMPENSATION_FAILED (~14s)
./bin/forge sagas list
# → status: COMPENSATION_FAILED
```

**Observe:** `COMPENSATION_FAILED` state is persistent across conductor restarts. The saga is dead-lettered — manual intervention required.

---

### Demo 6 — ListSagas + Step Detail

**Features:** `ListSagas` (status filter, pagination), `GetSaga` (steps, `error_detail`, `failed_step`).

```bash
# Run two sagas concurrently
./bin/forge create --owner demo6a --dry-run &
./bin/forge create --owner demo6b --dry-run &
sleep 3

# List running sagas (ListSagas with status filter)
./bin/forge sagas list --status=RUNNING

# Step-level detail (GetSaga + error_detail parsing)
./bin/forge status <demo6a-env-prefix>

# Pagination
./bin/forge sagas list --page-size=1
# → Next page: --cursor=<token>
./bin/forge sagas list --page-size=1 --cursor=<token>

# Wait for both to complete
./bin/forge list
```

**Observe:** `ListSagas` returns only sagas matching the status filter. Cursor-based pagination works across requests. Step timestamps and durations visible in `forge status`.

---

### Demo 7 — AbortSaga

**Features:** `AbortSaga` RPC, `ABORTED` terminal state (no compensation).

```bash
# Start provisioning (rds step has 8s delay — gives time to abort)
./bin/forge create --owner demo7 --dry-run &
sleep 3  # rds step is mid-execution

PREFIX=$(./bin/forge list | awk 'NR>1 && $2=="demo7" {print $1}')

# Abort the saga (AbortSaga RPC — no compensation triggered)
./bin/forge sagas abort "$PREFIX"
# → Saga <id> aborted — status: SAGA_STATUS_ABORTED

./bin/forge sagas list
./bin/forge status "$PREFIX"
```

**Observe:** Saga moves to `ABORTED` state immediately. No compensation steps are triggered (unlike Demo 2). The environment stays in `provisioning` status in BoltDB.

---

### Demo 8 — Metrics + SSE Dashboard

**Features:** Prometheus `Recorder` interface, `Observer`/SSE interface, `/metrics`, `/dashboard/events`.

```bash
./bin/forge create --owner demo8 --dry-run &
sleep 2

# Prometheus metrics (saga execution counters + step durations)
curl -sf http://localhost:8081/metrics | grep -E '^(saga|step)_'
# → saga_executions_total{status="running"} 1
# → step_duration_seconds_bucket{step="vpc",...}

# SSE dashboard — real-time saga state transitions (streams for 5s)
curl -sN http://localhost:8081/dashboard/events
# → data: {"id":"abc12345","status":"RUNNING","steps":[...]}
# → data: {"id":"abc12345","status":"RUNNING","steps":[{"name":"vpc","status":"SUCCEEDED"},...]}

wait  # wait for saga to complete
```

**Observe:** Prometheus counters increment as steps complete. SSE stream delivers a new event on each state transition, showing step-by-step progression.

---

### Demo 9 — Graceful Drain

**Features:** `Drain()` (in-flight saga preservation), `Resume()` (crash restart), SIGTERM handling.

```bash
./bin/forge create --owner demo9 --dry-run &
sleep 3  # rds step mid-execution (~5s remaining)

# Send SIGTERM to conductor — Drain() waits for in-flight saga to complete
DRAIN_START=$(date +%s)
kill -SIGTERM <conductor-pid>
wait <conductor-pid>  # blocks until drained
DRAIN_END=$(date +%s)
echo "Drain took $((DRAIN_END - DRAIN_START))s"

# Restart conductor — Resume() re-drives any remaining steps
start_conductor
./bin/forge list
# → status: ready  (saga completed — either during drain or after Resume())
```

**Observe:** Drain duration (~5–14s) proves the conductor waited for the in-flight saga. After restart, `Resume()` picks up any remaining work from BoltDB.

---

### Demo 10 — Idempotency Key + Saga Timeout

**Features:** `idempotency_key` in `CreateSagaRequest`, `saga_timeout_seconds=300`, per-step `timeout_seconds`/`max_retries`/`retry_backoff_ms`.

```bash
./bin/forge create --owner demo10 --dry-run

# forge-api sends idempotency_key="env-<uuid>" on every CreateSaga
grep -i "idempotency" /tmp/demo-logs/conductor.log | tail -5
# → INF saga created idempotency_key=env-<uuid> saga_id=abc12345

# Per-step timeout and retry configuration
grep -i "timeout\|max_retries" /tmp/demo-logs/conductor.log | head -10
# vpc: timeout=30s max_retries=2 backoff=500ms
# rds: timeout=600s max_retries=3 backoff=2000ms
# health: timeout=60s max_retries=1
```

**Observe:** The `idempotency_key` prevents duplicate sagas if forge-api retries `CreateSaga` due to a network blip. Per-step timeouts and backoffs are configured per step type — RDS gets a longer timeout to handle real provisioning latency.

---

## Minikube Mode (svid-exchange features)

### Demo 3 — Policy Lifecycle

**Features:** `CreatePolicy`, `DeletePolicy`, `ListPolicies` — dynamic policies tied to environment lifecycle.

```bash
# Show policies before provisioning
./bin/forge policies list --worker-url=http://localhost:9091

# Provision — identity step calls CreatePolicy
./bin/forge create --owner demo3a --dry-run
./bin/forge policies list --worker-url=http://localhost:9091
# → policy-env-<id8> now visible (created by Step 5)

# Trigger compensation cascade — identity step compensation calls DeletePolicy
./bin/forge create --owner demo3b --dry-run --fail-at-health
./bin/forge policies list --worker-url=http://localhost:9091
# → policy for demo3b is gone (compensation deleted it)
```

**Observe:** Policies are created atomically with the environment and deleted on compensation. The full svid-exchange CRUD lifecycle (Create → List → Delete) is driven by the saga.

---

### Demo 4 — JWT Token Exchange

**Features:** `Exchange` RPC, ES256 JWT, scope request, `granted_scopes`, `token_id`.

```bash
# Watch forge-worker logs while provisioning
kubectl logs -f -l app=forge-worker &

./bin/forge create --owner demo4 --dry-run
```

**In the forge-worker logs you'll see:**
```
INF JWT token exchange enabled svid_exchange_addr=svid-exchange...:8080
INF JWT exchanged token_id=<jti> expires_at=<unix-ts>
```

Each step's state call to forge-api includes `Authorization: Bearer <JWT>`. forge-api validates the JWT against svid-exchange's JWKS endpoint before allowing the BoltDB write.

**Observe:** forge-worker presents its SPIFFE X509 SVID to svid-exchange via mTLS. svid-exchange verifies the SVID against the registered policy, mints an ES256 JWT with the requested scopes, and returns it. forge-api verifies the JWT signature and scope before processing the request.

---

### Demo 11 — Policy Lifecycle + ReloadPolicy + gRPC Reflection

**Features:** `CreatePolicy`, `ListPolicies`, `ReloadPolicy`, `grpc_reflection=true`.

```bash
# Provision an environment
./bin/forge create --owner demo11 --dry-run

# List all policies (YAML-sourced + dynamic)
./bin/forge policies list --worker-url=http://localhost:9091
# → forge-worker-to-forge-api   yaml     ...sa/forge-worker  ...sa/forge-api  env:read,env:write  3600s
# → policy-env-<id8>            dynamic  ...env/app           ...env/db-proxy  read,write          3600s

# Reload YAML policies without restart
./bin/forge policies reload --worker-url=http://localhost:9091
# → Policies reloaded successfully.

# gRPC reflection — discover services without proto files
grpcurl -plaintext localhost:8082 list
# → admin.v1.PolicyAdmin
# → grpc.reflection.v1alpha.ServerReflection
grpcurl -plaintext localhost:8082 admin.v1.PolicyAdmin/ListPolicies
```

**Observe:** `grpc_reflection=true` allows grpcurl to discover and call the admin API without proto files. `ReloadPolicy` applies YAML changes at runtime. Dynamic policies from the identity step persist across reloads.

---

### Demo 12 — Token Revocation

**Features:** `RevokeToken`, `ListRevokedTokens`, BoltDB-persisted revocation.

```bash
# Get a JWT token ID from forge-worker logs
kubectl logs -l app=forge-worker | grep token_id
# → INF JWT exchanged token_id=abc-jti-xyz expires_at=1711234567

# Revoke the token (persisted in svid-exchange BoltDB)
./bin/forge tokens revoke abc-jti-xyz \
  --worker-url=http://localhost:9091 \
  --expires-at=1711234567

# Confirm revocation
./bin/forge tokens revoked --worker-url=http://localhost:9091
# → TOKEN ID      EXPIRES AT
# → abc-jti-xyz   2026-03-26T11:00:00Z
```

**Observe:** Revoked tokens are rejected by svid-exchange even before their natural expiry. The revocation list survives svid-exchange restarts via BoltDB persistence. The `expires-at` timestamp is when to purge the revocation record (after that the token would be expired anyway).

---

### Demo 13 — Scope Enforcement

**Features:** `NewMiddleware`, `ClaimsFromContext`, `HasScope`, `HasAllScopes`.

```bash
./bin/forge create --owner demo13 --dry-run &
sleep 5

# forge-api logs show scope validation for internal routes
kubectl logs -l app=forge-api --namespace=default | \
  grep -E "(scope|authorized|denied)" | tail -10
# → INF internal GET authorized sub=spiffe://.../forge-worker scope=env:read
# → INF internal PUT authorized sub=spiffe://.../forge-worker scope=env:read env:write

wait  # wait for saga to complete
```

**forge-api enforces:**
- `GET /internal/envs/{id}` — requires `env:read` scope
- `PUT /internal/envs/{id}` — requires both `env:read` and `env:write` scopes

The forge-worker-to-forge-api policy grants `["env:read", "env:write"]`, so normal provisioning succeeds. A JWT missing the required scope would get `403 Forbidden`.

**Observe:** `svidclient.NewMiddleware` extracts JWT claims from the request. `ClaimsFromContext` retrieves them inside the handler. `HasAllScopes` gates the write path.

---

### Demo 14 — Key Rotation + Rate Limiting

**Features:** `key_rotation_interval`, `rate_limit_rps`/`rate_limit_burst`, `AUDIT_HMAC_KEY`, JWKS endpoint.

```bash
# Check key rotation logs
kubectl logs -l app=svid-exchange --namespace=default | \
  grep -E "(rotation|rate_limit|audit)" | head -10
# → INF signing key rotated new_key_id=k2 old_key_id=k1

# JWKS endpoint — shows all active public keys (including rotated ones)
# Port 8083 = port-forward for svid-exchange:8081 (JWKS/health)
curl -sf http://localhost:8083/jwks | python3 -m json.tool
# → {"keys": [{"kid": "k2", "kty": "EC", "crv": "P-256", ...}]}

# Rate limiting and audit config set in server.yaml ConfigMap:
# rate_limit_rps: 50, rate_limit_burst: 10
# AUDIT_HMAC_KEY: <secret> (enables tamper-evident audit log HMAC)
kubectl logs -l app=svid-exchange | grep audit
# → {"level":"info","event":"exchange","sub":"spiffe://...","hmac":"<sha256>"}
```

**Zero-downtime key rotation flow:**
1. svid-exchange rotates the signing key every `key_rotation_interval`
2. Old key stays in JWKS until all tokens signed with it expire
3. forge-api's JWKS auto-refresh (every 5 minutes) picks up the new key
4. forge-worker's background token refresh (at 80% TTL) re-fetches tokens signed with new key

**Observe:** JWKS endpoint returns the current public key set. AUDIT_HMAC_KEY enables tamper-evident audit events — each log line includes an HMAC that proves it wasn't modified after the fact.
