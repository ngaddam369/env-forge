package steps

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // postgres driver — imported for side-effects
	"github.com/ngaddam369/env-forge/internal/environment"
)

// HealthStep validates that the provisioned environment is fully functional:
//  1. Connects to the RDS endpoint and runs SELECT 1.
//  2. (Placeholder) Checks svid-exchange JWKS endpoint reachability.
//
// This step has no compensation — it is read-only. If it fails, saga-conductor
// triggers compensations starting from Step 6 backward.
//
// FailAtHealth on the environment injects a synthetic failure for demo moment 2.
type HealthStep struct{}

// NewHealthStep creates a HealthStep.
func NewHealthStep() *HealthStep {
	return &HealthStep{}
}

func (s *HealthStep) Name() string { return "health" }

func (s *HealthStep) Execute(ctx context.Context, env *environment.Environment, _ *environment.Store) error {
	// Demo moment 2: inject failure for compensation cascade demo.
	if env.FailAtHealth {
		return fmt.Errorf("injected health failure for demo (--fail-at-health)")
	}

	if env.DryRun {
		time.Sleep(1 * time.Second)
		return nil
	}

	// Check 1: RDS connectivity.
	if err := checkRDS(ctx, env.RDSEndpoint, env.RDSPassword); err != nil {
		return fmt.Errorf("rds health check failed: %w", err)
	}

	return nil
}

func checkRDS(ctx context.Context, endpoint, password string) error {
	dsn := fmt.Sprintf("postgres://forgeadmin:%s@%s/postgres?sslmode=require&connect_timeout=10", password, endpoint)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close() //nolint:errcheck

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	var result int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&result); err != nil {
		return fmt.Errorf("select 1: %w", err)
	}
	return nil
}

// Compensate is a no-op — health validation is read-only.
func (s *HealthStep) Compensate(_ context.Context, _ *environment.Environment, _ *environment.Store) error {
	return nil
}

func (s *HealthStep) IsAlreadyDone(context.Context, *environment.Environment) (bool, error) {
	// Health checks are idempotent — always re-run to verify current state.
	return false, nil
}
