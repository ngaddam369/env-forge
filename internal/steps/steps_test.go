package steps_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/steps"
)

// newTestStore creates a temporary BoltDB store for test isolation.
func newTestStore(t *testing.T) *environment.Store {
	t.Helper()
	store, err := environment.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	return store
}

// newDryRunEnv returns a dry-run environment with a predictable ID for tests.
func newDryRunEnv(id string) *environment.Environment {
	return &environment.Environment{
		ID:        id + "-aaaa-bbbb-cccc",
		Owner:     "test",
		Status:    environment.StatusProvisioning,
		DryRun:    true,
		CreatedAt: time.Now().UTC(),
	}
}

// ── Execute / Compensate cycle ────────────────────────────────────────────────

// stepCase describes a single Execute→Compensate cycle for a dry-run step.
type stepCase struct {
	name string
	step steps.Step
	// setup is called on the env before Execute (e.g. to pre-populate fields).
	setup func(env *environment.Environment)
	// checkAfterExecute asserts fields that must be set after Execute succeeds.
	checkAfterExecute func(t *testing.T, env *environment.Environment)
	// checkAfterCompensate asserts fields that must be cleared after Compensate.
	checkAfterCompensate func(t *testing.T, env *environment.Environment)
}

func TestStep_ExecuteAndCompensate(t *testing.T) {
	tmpDir := t.TempDir()

	cases := []stepCase{
		{
			name: "vpc",
			step: steps.NewVPCStep(nil),
			checkAfterExecute: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.VPCID == "" {
					t.Error("VPCID should be set after Execute")
				}
			},
			checkAfterCompensate: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.VPCID != "" {
					t.Errorf("VPCID should be cleared after Compensate, got %q", env.VPCID)
				}
			},
		},
		{
			name: "rds",
			step: steps.NewRDSStep(nil),
			checkAfterExecute: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.RDSEndpoint == "" {
					t.Error("RDSEndpoint should be set after Execute")
				}
				if env.RDSPassword == "" {
					t.Error("RDSPassword should be set after Execute")
				}
			},
			checkAfterCompensate: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.RDSEndpoint != "" {
					t.Errorf("RDSEndpoint should be cleared after Compensate, got %q", env.RDSEndpoint)
				}
			},
		},
		{
			name: "ec2",
			step: steps.NewEC2Step(nil),
			checkAfterExecute: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.EC2InstanceID == "" {
					t.Error("EC2InstanceID should be set after Execute")
				}
			},
			checkAfterCompensate: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.EC2InstanceID != "" {
					t.Errorf("EC2InstanceID should be cleared after Compensate, got %q", env.EC2InstanceID)
				}
			},
		},
		{
			name: "s3",
			step: steps.NewS3Step(nil, "us-east-1"),
			checkAfterExecute: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.S3BucketName == "" {
					t.Error("S3BucketName should be set after Execute")
				}
			},
			checkAfterCompensate: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.S3BucketName != "" {
					t.Errorf("S3BucketName should be cleared after Compensate, got %q", env.S3BucketName)
				}
			},
		},
		{
			name: "identity",
			step: steps.NewIdentityStep(nil, "cluster.local"),
			checkAfterExecute: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if len(env.SPIREEntryIDs) == 0 {
					t.Error("SPIREEntryIDs should be set after Execute")
				}
				if env.SVIDExchangePolicyName == "" {
					t.Error("SVIDExchangePolicyName should be set after Execute")
				}
			},
			checkAfterCompensate: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.SVIDExchangePolicyName != "" {
					t.Errorf("SVIDExchangePolicyName should be cleared after Compensate, got %q", env.SVIDExchangePolicyName)
				}
			},
		},
		{
			name: "config",
			step: steps.NewConfigStep(nil, "svid-exchange:8080", "cluster.local", tmpDir),
			setup: func(env *environment.Environment) {
				env.RDSEndpoint = "db.example.com:5432"
				env.RDSPassword = "secret"
				env.S3BucketName = "my-bucket"
			},
			checkAfterExecute: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.LocalEnvPath == "" {
					t.Error("LocalEnvPath should be set after Execute")
				}
			},
			checkAfterCompensate: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.LocalEnvPath != "" {
					t.Errorf("LocalEnvPath should be cleared after Compensate, got %q", env.LocalEnvPath)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			prefix := tc.name
			if len(prefix) > 4 {
				prefix = prefix[:4]
			}
			env := newDryRunEnv(prefix + "test")
			if tc.setup != nil {
				tc.setup(env)
			}
			_ = store.Put(env)

			if tc.step.Name() != tc.name {
				t.Errorf("step.Name()=%q, want %q", tc.step.Name(), tc.name)
			}

			// Execute
			if err := tc.step.Execute(context.Background(), env, store); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			tc.checkAfterExecute(t, env)

			// Verify persistence in store.
			reloaded, err := store.Get(env.ID)
			if err != nil {
				t.Fatalf("reload from store: %v", err)
			}
			tc.checkAfterExecute(t, reloaded)

			// Compensate
			if err := tc.step.Compensate(context.Background(), env, store); err != nil {
				t.Fatalf("Compensate: %v", err)
			}
			tc.checkAfterCompensate(t, env)
		})
	}
}

// ── IsAlreadyDone idempotency ─────────────────────────────────────────────────

func TestStep_IsAlreadyDone_Idempotency(t *testing.T) {
	cases := []struct {
		name     string
		step     steps.Step
		setup    func(env *environment.Environment) // pre-populate the "done" field
		wantDone bool
	}{
		{
			name: "vpc already done",
			step: steps.NewVPCStep(nil),
			setup: func(env *environment.Environment) {
				env.VPCID = "vpc-already"
			},
			wantDone: true,
		},
		{
			name:     "vpc not yet done",
			step:     steps.NewVPCStep(nil),
			wantDone: false,
		},
		{
			name: "identity already done",
			step: steps.NewIdentityStep(nil, "cluster.local"),
			setup: func(env *environment.Environment) {
				env.SVIDExchangePolicyName = "policy-env-12345678"
			},
			wantDone: true,
		},
		{
			name:     "identity not yet done",
			step:     steps.NewIdentityStep(nil, "cluster.local"),
			wantDone: false,
		},
		{
			name: "registry already done",
			step: steps.NewRegistryStep(),
			setup: func(env *environment.Environment) {
				env.Status = environment.StatusReady
			},
			wantDone: true,
		},
		{
			name:     "registry not yet done",
			step:     steps.NewRegistryStep(),
			wantDone: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDryRunEnv("idemp001")
			if tc.setup != nil {
				tc.setup(env)
			}

			done, err := tc.step.IsAlreadyDone(context.Background(), env)
			if err != nil {
				t.Fatalf("IsAlreadyDone: %v", err)
			}
			if done != tc.wantDone {
				t.Errorf("done=%v, want %v", done, tc.wantDone)
			}
		})
	}
}

// ── HealthStep ────────────────────────────────────────────────────────────────

// TestHealthStep covers the FailAtHealth flag and the normal dry-run path as a
// table since both share the same step instance and store/env setup.
func TestHealthStep(t *testing.T) {
	cases := []struct {
		name         string
		failAtHealth bool
		wantErr      bool
	}{
		{name: "dry-run succeeds", failAtHealth: false, wantErr: false},
		{name: "FailAtHealth returns error", failAtHealth: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			env := newDryRunEnv("hlthtest")
			env.FailAtHealth = tc.failAtHealth
			step := steps.NewHealthStep()

			err := step.Execute(context.Background(), env, store)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Compensate is always a no-op regardless of FailAtHealth.
			if err := step.Compensate(context.Background(), env, store); err != nil {
				t.Fatalf("Compensate: %v", err)
			}
		})
	}
}

// ── RegistryStep ──────────────────────────────────────────────────────────────

func TestRegistryStep_ExecuteAndCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("regtest1")
	_ = store.Put(env)

	step := steps.NewRegistryStep()

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if env.Status != environment.StatusReady {
		t.Errorf("expected status=ready after Execute, got %s", env.Status)
	}

	done, err := step.IsAlreadyDone(context.Background(), env)
	if err != nil || !done {
		t.Errorf("expected IsAlreadyDone=true after Execute: done=%v err=%v", done, err)
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	// Compensation marks the record destroyed rather than deleting it.
	updated, err := store.Get(env.ID)
	if err != nil {
		t.Fatalf("env should still exist after compensation: %v", err)
	}
	if updated.Status != environment.StatusDestroyed {
		t.Errorf("expected status=destroyed after Compensate, got %s", updated.Status)
	}
}
