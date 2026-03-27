package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	svidclient "github.com/ngaddam369/svid-exchange/pkg/client"

	"github.com/ngaddam369/env-forge/internal/apiserver"
	"github.com/ngaddam369/env-forge/internal/conductor"
	"github.com/ngaddam369/env-forge/internal/environment"
	pb "github.com/ngaddam369/saga-conductor/proto/saga/v1"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		log.Fatal().Err(err).Msg("forge-api exited")
	}
}

func run(ctx context.Context) error {
	addr := envOrDefault("STEP_ADDR", ":9090")
	dbPath := envOrDefault("DB_PATH", "env-forge.db")
	conductorAddr := envOrDefault("CONDUCTOR_ADDR", "saga-conductor.default.svc.cluster.local:8080")
	workerURL := envOrDefault("FORGE_WORKER_URL", "http://forge-worker.default.svc.cluster.local:9091")
	jwksURL := envOrDefault("SVIDEXCHANGE_JWKS_URL", "")
	trustDomain := envOrDefault("TRUST_DOMAIN", "cluster.local")

	store, err := environment.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			log.Error().Err(cerr).Msg("close store")
		}
	}()

	c, err := conductor.New(conductorAddr, workerURL)
	if err != nil {
		return fmt.Errorf("connect to conductor: %w", err)
	}
	defer func() {
		if cerr := c.Close(); cerr != nil {
			log.Error().Err(cerr).Msg("close conductor client")
		}
	}()

	// JWT verifier — nil disables validation (dev mode).
	// Uses *svidclient.Verifier directly so apiserver can pass it to
	// svid-exchange NewMiddleware, enabling ClaimsFromContext + scope checks.
	var verifier *svidclient.Verifier
	if jwksURL != "" {
		for attempt := 1; attempt <= 10; attempt++ {
			verifier, err = svidclient.NewVerifier(ctx, jwksURL)
			if err == nil {
				break
			}
			log.Warn().Err(err).Msgf("JWKS fetch failed (attempt %d/10) — retrying in 3s", attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		if err != nil {
			log.Warn().Err(err).Msg("JWKS unavailable after retries — JWT validation disabled")
			verifier = nil
		} else {
			verifier.StartAutoRefresh(ctx, 5*time.Minute)
			log.Info().Str("jwks_url", jwksURL).Msg("JWT verification enabled (svid-exchange NewMiddleware + scope checks)")
		}
	} else {
		log.Warn().Msg("SVIDEXCHANGE_JWKS_URL not set — JWT validation disabled (dev mode)")
	}

	provisioner := &conductorProvisioner{c: c, store: store}
	audience := fmt.Sprintf("spiffe://%s/ns/default/sa/forge-api", trustDomain)

	srv := apiserver.New(store, provisioner, verifier, audience, c, log.Logger)
	log.Info().Str("addr", addr).Msg("forge-api listening")
	return srv.ListenAndServe(ctx, addr)
}

// conductorProvisioner adapts *conductor.Client to apiserver.Provisioner.
// It creates the saga first, persists the saga ID so forge status can show step
// detail, then starts the saga and blocks until it reaches a terminal state.
type conductorProvisioner struct {
	c     *conductor.Client
	store *environment.Store
}

func (p *conductorProvisioner) Provision(ctx context.Context, env *environment.Environment) error {
	sagaID, err := p.c.CreateEnvSaga(ctx, env)
	if err != nil {
		return err
	}
	env.SagaID = sagaID
	if err := p.store.Put(env); err != nil {
		return fmt.Errorf("persist saga ID: %w", err)
	}
	exec, err := p.c.StartEnvSaga(ctx, sagaID)
	if err != nil {
		return err
	}
	// engine.Start returns (exec, nil) for FAILED sagas (compensation succeeded).
	// The registry step only runs on success, so we must explicitly mark the env
	// as failed here — otherwise handleCreateEnv's error path never fires and the
	// env stays in "provisioning" forever.
	if exec != nil && exec.Status == pb.SagaStatus_SAGA_STATUS_FAILED {
		return fmt.Errorf("saga failed: step %q failed, compensation complete", exec.FailedStep)
	}
	return nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
