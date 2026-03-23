package steps

import (
	"context"

	"github.com/ngaddam369/env-forge/internal/environment"
)

// Step is the interface every saga step must implement.
//
// Execute performs the forward action (provision a resource).
// Compensate undoes a previously successful Execute (tear down a resource).
// IsAlreadyDone checks whether the step was already completed — critical for
// idempotent crash recovery: saga-conductor may re-run a step after a restart.
//
// All implementations must be safe for concurrent calls; in practice the engine
// runs steps sequentially, but tests may call them concurrently.
type Step interface {
	Name() string
	Execute(ctx context.Context, env *environment.Environment, store *environment.Store) error
	Compensate(ctx context.Context, env *environment.Environment, store *environment.Store) error
	IsAlreadyDone(ctx context.Context, env *environment.Environment) (bool, error)
}
