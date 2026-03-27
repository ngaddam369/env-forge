# CLI Reference

All commands use the `forge` binary built from `cmd/forge/`.

```bash
cd ~/go_projects/env-forge
mise install && make build
```

## Environment Commands

### `forge create`

Provisions a new environment. Submits the saga to saga-conductor; polls until terminal state.

```bash
./bin/forge create --owner <name> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--owner` | (required) | Owner name for the environment |
| `--dry-run` | false | Use fake IDs and sleep delays instead of real AWS |
| `--fail-at-health` | false | Force health step to fail (for Demo 2 compensation cascade) |
| `--forge-url` | `http://localhost:9090` | forge-api base URL |

```bash
# Dry-run (no AWS credentials needed, ~21s)
./bin/forge create --owner alice --dry-run

# Force compensation cascade (for testing)
./bin/forge create --owner alice --dry-run --fail-at-health

# Point at a different forge-api
./bin/forge create --owner alice --dry-run --forge-url http://localhost:9090
```

### `forge list`

Lists all environments from BoltDB.

```bash
./bin/forge list [--forge-url URL]
```

Output columns: `ID  OWNER  STATUS  DRY-RUN  CREATED`

```
ID        OWNER   STATUS         DRY-RUN  CREATED
a1b2c3d4  alice   ready          true     2026-03-26T10:00:00Z
e5f6a7b8  bob     provisioning   true     2026-03-26T10:00:05Z
```

### `forge status`

Shows step-level detail for a specific environment, including saga state from saga-conductor.

```bash
./bin/forge status <env-id-prefix> [--forge-url URL]
```

The `env-id-prefix` is the 8-character ID prefix shown in `forge list`.

Output includes:
- Environment ID, owner, status, dry-run flag
- Saga ID, saga status, failed step (if any)
- Per-step: name, status, start time, completion time, error detail

### `forge destroy`

Destroys an environment by triggering the compensation path.

```bash
./bin/forge destroy <env-id-prefix> [--forge-url URL]
```

---

## Saga Commands

These commands interact directly with saga-conductor via forge-api.

### `forge sagas list`

Lists sagas using saga-conductor's `ListSagas` RPC with optional status filter and pagination.

```bash
./bin/forge sagas list [flags]
```

| Flag | Description |
|------|-------------|
| `--status` | Filter by saga status (`RUNNING`, `COMPLETED`, `FAILED`, `COMPENSATING`, `ABORTED`, `COMPENSATION_FAILED`) |
| `--page-size` | Number of results per page (default: 20) |
| `--cursor` | Pagination cursor from a previous `Next page: --cursor=<token>` line |

```bash
# List all sagas
./bin/forge sagas list

# Filter by status
./bin/forge sagas list --status=RUNNING
./bin/forge sagas list --status=COMPENSATION_FAILED

# Paginate
./bin/forge sagas list --page-size=1
# → Next page: --cursor=<token>
./bin/forge sagas list --page-size=1 --cursor=<token>
```

### `forge sagas abort`

Aborts a running saga using saga-conductor's `AbortSaga` RPC. Moves the saga to ABORTED state without triggering compensation.

```bash
./bin/forge sagas abort <env-id-prefix> [--forge-url URL]
```

Output: `Saga <id> aborted — status: SAGA_STATUS_ABORTED`

---

## Policy Commands (svid-exchange)

Policy commands route through forge-worker's HTTP admin proxy, which uses SPIFFE mTLS to reach svid-exchange's admin gRPC endpoint (port 8082).

### `forge policies list`

Lists all exchange policies (YAML-sourced + dynamically created by the identity step).

```bash
./bin/forge policies list --worker-url=http://localhost:9091
```

Output columns: `NAME  SOURCE  SUBJECT  TARGET  SCOPES  MAX TTL`

### `forge policies reload`

Triggers svid-exchange's `ReloadPolicy` RPC — reloads the YAML policy file without restart.

```bash
./bin/forge policies reload --worker-url=http://localhost:9091
```

---

## Token Commands (svid-exchange)

### `forge tokens revoke`

Revokes a JWT by its `jti` claim. The revocation is persisted in svid-exchange's BoltDB and survives restarts.

```bash
./bin/forge tokens revoke <token-jti> \
  --worker-url=http://localhost:9091 \
  --expires-at=<unix-timestamp>
```

### `forge tokens revoked`

Lists all revoked tokens from svid-exchange's revocation store.

```bash
./bin/forge tokens revoked --worker-url=http://localhost:9091
```

Output columns: `TOKEN ID  EXPIRES AT`

---

## Global Flags

All commands accept:

| Flag | Default | Description |
|------|---------|-------------|
| `--forge-url` | `http://localhost:9090` | forge-api base URL |
| `--worker-url` | `http://localhost:9091` | forge-worker base URL (policy/token commands) |
