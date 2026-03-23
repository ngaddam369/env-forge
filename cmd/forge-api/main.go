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
	defer store.Close() //nolint:errcheck

	c, err := conductor.New(conductorAddr, workerURL)
	if err != nil {
		return fmt.Errorf("connect to conductor: %w", err)
	}
	defer c.Close() //nolint:errcheck

	var verifier apiserver.Verifier
	if jwksURL != "" {
		var raw *svidclient.Verifier
		for attempt := 1; attempt <= 10; attempt++ {
			raw, err = svidclient.NewVerifier(ctx, jwksURL)
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
		} else {
			raw.StartAutoRefresh(ctx, 5*time.Minute)
			verifier = &jwtVerifier{v: raw}
			log.Info().Str("jwks_url", jwksURL).Msg("JWT verification enabled")
		}
	} else {
		log.Warn().Msg("SVIDEXCHANGE_JWKS_URL not set — JWT validation disabled (dev mode)")
	}

	provisioner := &conductorProvisioner{c: c}
	audience := fmt.Sprintf("spiffe://%s/ns/default/sa/forge-api", trustDomain)
	srv := apiserver.New(store, provisioner, verifier, audience, log.Logger)
	log.Info().Str("addr", addr).Msg("forge-api listening")
	return srv.ListenAndServe(ctx, addr)
}

// conductorProvisioner adapts *conductor.Client to apiserver.Provisioner.
type conductorProvisioner struct{ c *conductor.Client }

func (p *conductorProvisioner) Provision(ctx context.Context, env *environment.Environment) error {
	_, err := p.c.Provision(ctx, env)
	return err
}

// jwtVerifier wraps *svidclient.Verifier to satisfy apiserver.Verifier.
type jwtVerifier struct{ v *svidclient.Verifier }

func (j *jwtVerifier) Verify(token, audience string) error {
	_, err := j.v.Verify(token, audience)
	return err
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
