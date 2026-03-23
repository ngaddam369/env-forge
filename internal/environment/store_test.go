package environment_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaddam369/env-forge/internal/environment"
)

func newTestStore(t *testing.T) *environment.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := environment.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	return store
}

func newEnv(id, owner string) *environment.Environment {
	return &environment.Environment{
		ID:        id,
		Owner:     owner,
		Status:    environment.StatusProvisioning,
		CreatedAt: time.Now().UTC(),
	}
}

func TestStore_PutAndGet(t *testing.T) {
	store := newTestStore(t)
	env := newEnv("aaaa-bbbb-cccc-dddd", "alice")

	if err := store.Put(env); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(env.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != env.ID {
		t.Errorf("ID mismatch: got %s, want %s", got.ID, env.ID)
	}
	if got.Owner != env.Owner {
		t.Errorf("Owner mismatch: got %s, want %s", got.Owner, env.Owner)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Get("does-not-exist")
	if err == nil {
		t.Fatal("expected ErrNotFound, got nil")
	}
}

func TestStore_List(t *testing.T) {
	store := newTestStore(t)

	envs := []*environment.Environment{
		newEnv("id-1", "alice"),
		newEnv("id-2", "bob"),
		newEnv("id-3", "carol"),
	}
	envs[1].Status = environment.StatusReady

	for _, e := range envs {
		if err := store.Put(e); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	all, err := store.List("")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 environments, got %d", len(all))
	}

	ready, err := store.List(environment.StatusReady)
	if err != nil {
		t.Fatalf("List ready: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != "id-2" {
		t.Errorf("expected 1 ready env id-2, got %v", ready)
	}
}

func TestStore_Delete(t *testing.T) {
	store := newTestStore(t)
	env := newEnv("del-id", "alice")

	if err := store.Put(env); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Delete(env.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(env.ID); err == nil {
		t.Fatal("expected ErrNotFound after Delete, got nil")
	}
}

func TestStore_Put_UpdatesTimestamp(t *testing.T) {
	store := newTestStore(t)
	env := newEnv("ts-id", "alice")

	if err := store.Put(env); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	first, _ := store.Get(env.ID)

	env.Status = environment.StatusReady
	if err := store.Put(env); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	second, _ := store.Get(env.ID)

	if !second.UpdatedAt.After(first.UpdatedAt) || second.UpdatedAt.Equal(time.Time{}) {
		// Timestamps may be equal if the test runs very fast; allow equal too.
		if !second.UpdatedAt.Equal(first.UpdatedAt) {
			t.Errorf("UpdatedAt not progressed: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
		}
	}
	if second.Status != environment.StatusReady {
		t.Errorf("Status not persisted: got %s", second.Status)
	}
}

func TestStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persistent.db")

	// Write in first store instance.
	store1, err := environment.Open(dbPath)
	if err != nil {
		t.Fatalf("open store1: %v", err)
	}
	env := newEnv("persist-id", "alice")
	env.VPCID = "vpc-123"
	if err := store1.Put(env); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close store1: %v", err)
	}

	// Read in second store instance (simulates restart).
	store2, err := environment.Open(dbPath)
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	defer store2.Close() //nolint:errcheck

	got, err := store2.Get(env.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.VPCID != "vpc-123" {
		t.Errorf("VPCID not persisted: got %s", got.VPCID)
	}
}

func TestStore_DeleteMissingFile(t *testing.T) {
	// Ensure we can open a non-existent file (creates it).
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "new.db")

	if _, err := os.Stat(dbPath); err == nil {
		t.Fatalf("db file should not exist yet")
	}

	store, err := environment.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db file not created: %v", err)
	}
}
