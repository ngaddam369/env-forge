# Development

## Toolchain

Go version is pinned to 1.26.1 via `mise.toml`. Install with:

```bash
mise install
```

## Make Targets

```bash
# Install Go toolchain and dependencies
mise install

# Run all checks in order (build → vet → lint → test)
make verify

# Individual targets
make build    # compile forge-api, forge-worker, forge binaries → bin/
make vet      # go vet ./...
make lint     # golangci-lint v2 (must be zero issues)
make test     # race-detector tests + coverage report
```

Binaries are written to `bin/`:
- `bin/forge` — CLI
- `bin/forge-api` — API server
- `bin/forge-worker` — step executor

## Project Layout

```
cmd/
  forge/         — cobra CLI (create, destroy, list, status, sagas, policies, tokens)
  forge-api/     — HTTP API server + BoltDB store + saga-conductor client
  forge-worker/  — saga step HTTP server + svid-exchange client
internal/
  environment/   — Environment struct, BoltDB store, ErrNotFound
  steps/         — Step interface + 8 implementations (vpc, rds, ec2, s3, identity, config, health, registry)
  server/        — HTTP router for step endpoints + admin proxy
  conductor/     — saga-conductor gRPC client wrapper
  apiserver/     — forge-api HTTP handler (provision, list, status, abort)
  adminclient/   — svid-exchange admin gRPC client
  aws/           — AWS SDK client wrappers (EC2, RDS, S3)
k8s/
  forge-api.yaml
  forge-worker.yaml
  saga-conductor.yaml
  svid-exchange.yaml
  spire/
    spire-server.yaml
    spire-agent.yaml
scripts/
  register-spire-entries.sh
docs/           — this directory
```

## Adding a New Step

1. Create `internal/steps/mystep.go` implementing the `Step` interface:
   ```go
   type Step interface {
       Name() string
       Execute(ctx context.Context, env *environment.Environment) error
       Compensate(ctx context.Context, env *environment.Environment) error
       IsAlreadyDone(env *environment.Environment) bool
   }
   ```

2. Register in `cmd/forge-worker/main.go` `buildSteps()`:
   ```go
   steps.NewMyStep(...),
   ```

3. Register the step with saga-conductor in `internal/conductor/client.go` `CreateEnvSaga()`:
   ```go
   {Name: "mystep", ExecuteURL: selfURL + "/steps/mystep", CompensateURL: selfURL + "/steps/mystep/compensate", ...},
   ```

4. Add state field(s) to `internal/environment/types.go` if the step produces output.

5. Write tests in `internal/steps/mystep_test.go`.

## Dry-Run Mode

Steps check `env.DryRun` and skip real AWS calls when it's true. Instead they:
- Sleep a fixed delay to simulate work
- Write a deterministic fake ID (e.g., `fmt.Sprintf("vpc-dry-%s", env.ID[:8])`)

This allows full saga testing without AWS credentials.

## Testing

```bash
# All tests with race detector
make test

# Single package
cd ~/go_projects/env-forge
go test -race ./internal/steps/...
go test -race ./internal/server/...
```

Tests use real BoltDB instances in temp directories — no mocking of the store.

## Linting

golangci-lint v2 config is in `.golangci.yml`. The linter config uses `version: "2"` with `linters.default: none` and explicit enable lists.

```bash
make lint
```

Common lint errors:
- `errcheck`: always handle the error from `resp.Body.Close()` with a named return or explicit `_`
- `staticcheck`: replaces deprecated `gosimple` (merged in v2)

## CI

GitHub Actions runs on every PR to master:
- `format` — `gofmt -l`
- `vet` — `go vet`
- `lint` — golangci-lint v2
- `build` — `make build`
- `test` — `make test`

Branch protection requires all checks to pass before merge. Linear history is enforced (rebase only, no merge commits).
