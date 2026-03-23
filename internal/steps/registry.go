package steps

import (
	"context"
	"time"

	"github.com/ngaddam369/env-forge/internal/environment"
)

// RegistryStep writes the completed Environment record to the BoltDB store with
// status = "ready". This is the final step: it commits the environment as fully
// provisioned. Compensation marks it "destroyed" and removes it.
type RegistryStep struct{}

func NewRegistryStep() *RegistryStep { return &RegistryStep{} }

func (s *RegistryStep) Name() string { return "registry" }

func (s *RegistryStep) Execute(_ context.Context, env *environment.Environment, store *environment.Store) error {
	if env.DryRun {
		time.Sleep(200 * time.Millisecond)
	}
	env.Status = environment.StatusReady
	return store.Put(env)
}

func (s *RegistryStep) Compensate(_ context.Context, env *environment.Environment, store *environment.Store) error {
	env.Status = environment.StatusDestroyed
	if err := store.Put(env); err != nil {
		return err
	}
	return store.Delete(env.ID)
}

func (s *RegistryStep) IsAlreadyDone(_ context.Context, env *environment.Environment) (bool, error) {
	return env.Status == environment.StatusReady, nil
}
