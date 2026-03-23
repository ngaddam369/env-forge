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

// ── VPCStep ──────────────────────────────────────────────────────────────────

func TestVPCStep_DryRun_ExecuteCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("vpctest1")
	_ = store.Put(env)

	step := steps.NewVPCStep(nil)
	if step.Name() != "vpc" {
		t.Errorf("wrong name: %s", step.Name())
	}

	done, err := step.IsAlreadyDone(context.Background(), env)
	if err != nil || done {
		t.Errorf("expected not done before execute: done=%v err=%v", done, err)
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if env.VPCID == "" {
		t.Error("VPCID should be set after Execute")
	}

	// Reload from store to verify persistence.
	reloaded, _ := store.Get(env.ID)
	if reloaded.VPCID == "" {
		t.Error("VPCID not persisted in store")
	}

	done, err = step.IsAlreadyDone(context.Background(), env)
	if err != nil || !done {
		t.Errorf("expected done after execute: done=%v err=%v", done, err)
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if env.VPCID != "" {
		t.Error("VPCID should be cleared after Compensate")
	}
}

// ── RDSStep ───────────────────────────────────────────────────────────────────

func TestRDSStep_DryRun_ExecuteCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("rdstest1")
	_ = store.Put(env)

	step := steps.NewRDSStep(nil)
	if step.Name() != "rds" {
		t.Errorf("wrong name: %s", step.Name())
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if env.RDSEndpoint == "" || env.RDSPassword == "" {
		t.Error("RDS fields not set after Execute")
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if env.RDSEndpoint != "" {
		t.Error("RDSEndpoint should be cleared after Compensate")
	}
}

// ── EC2Step ───────────────────────────────────────────────────────────────────

func TestEC2Step_DryRun_ExecuteCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("ec2test1")
	_ = store.Put(env)

	step := steps.NewEC2Step(nil)
	if step.Name() != "ec2" {
		t.Errorf("wrong name: %s", step.Name())
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if env.EC2InstanceID == "" {
		t.Error("EC2InstanceID not set after Execute")
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if env.EC2InstanceID != "" {
		t.Error("EC2InstanceID should be cleared after Compensate")
	}
}

// ── S3Step ────────────────────────────────────────────────────────────────────

func TestS3Step_DryRun_ExecuteCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("s3test11")
	_ = store.Put(env)

	step := steps.NewS3Step(nil, "us-east-1")
	if step.Name() != "s3" {
		t.Errorf("wrong name: %s", step.Name())
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if env.S3BucketName == "" {
		t.Error("S3BucketName not set after Execute")
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if env.S3BucketName != "" {
		t.Error("S3BucketName should be cleared after Compensate")
	}
}

// ── IdentityStep ──────────────────────────────────────────────────────────────

func TestIdentityStep_DryRun_ExecuteCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("idtest11")
	_ = store.Put(env)

	step := steps.NewIdentityStep(nil, "cluster.local")
	if step.Name() != "identity" {
		t.Errorf("wrong name: %s", step.Name())
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(env.SPIREEntryIDs) == 0 {
		t.Error("SPIREEntryIDs not set after Execute")
	}
	if env.SVIDExchangePolicyName == "" {
		t.Error("SVIDExchangePolicyName not set after Execute")
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if env.SVIDExchangePolicyName != "" {
		t.Error("SVIDExchangePolicyName should be cleared after Compensate")
	}
}

// ── ConfigStep ────────────────────────────────────────────────────────────────

func TestConfigStep_DryRun_ExecuteCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("cfgtest1")
	env.RDSEndpoint = "db.example.com:5432"
	env.RDSPassword = "secret"
	env.S3BucketName = "my-bucket"
	_ = store.Put(env)

	tmpDir := t.TempDir()
	step := steps.NewConfigStep(nil, "svid-exchange:8080", "cluster.local", tmpDir)
	if step.Name() != "config" {
		t.Errorf("wrong name: %s", step.Name())
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if env.LocalEnvPath == "" {
		t.Error("LocalEnvPath not set after Execute")
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if env.LocalEnvPath != "" {
		t.Error("LocalEnvPath should be cleared after Compensate")
	}
}

// ── HealthStep ────────────────────────────────────────────────────────────────

func TestHealthStep_DryRun(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("hlthtest")
	step := steps.NewHealthStep()
	if step.Name() != "health" {
		t.Errorf("wrong name: %s", step.Name())
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Compensate is always a no-op.
	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
}

func TestHealthStep_FailAtHealth(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("failhlth")
	env.FailAtHealth = true
	step := steps.NewHealthStep()

	err := step.Execute(context.Background(), env, store)
	if err == nil {
		t.Fatal("expected error from FailAtHealth, got nil")
	}
}

// ── RegistryStep ──────────────────────────────────────────────────────────────

func TestRegistryStep_DryRun_ExecuteCompensate(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("regtest1")
	_ = store.Put(env)

	step := steps.NewRegistryStep()
	if step.Name() != "registry" {
		t.Errorf("wrong name: %s", step.Name())
	}

	if err := step.Execute(context.Background(), env, store); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if env.Status != environment.StatusReady {
		t.Errorf("Status should be ready, got %s", env.Status)
	}

	// IsAlreadyDone should return true.
	done, err := step.IsAlreadyDone(context.Background(), env)
	if err != nil || !done {
		t.Errorf("expected done=true, got done=%v err=%v", done, err)
	}

	if err := step.Compensate(context.Background(), env, store); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	// After compensation the record should be gone.
	if _, err := store.Get(env.ID); err == nil {
		t.Error("environment should be deleted from store after registry compensation")
	}
}

// ── Idempotency ───────────────────────────────────────────────────────────────

func TestVPCStep_IsAlreadyDone_Idempotent(t *testing.T) {
	store := newTestStore(t)
	env := newDryRunEnv("idemp001")
	env.VPCID = "vpc-already-set"
	_ = store.Put(env)

	step := steps.NewVPCStep(nil)

	// Should return done=true without calling Execute.
	done, err := step.IsAlreadyDone(context.Background(), env)
	if err != nil {
		t.Fatalf("IsAlreadyDone: %v", err)
	}
	if !done {
		t.Error("expected done=true when VPCID already set")
	}
}
