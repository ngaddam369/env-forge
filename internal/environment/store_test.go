package environment_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaddam369/env-forge/internal/environment"
)

// ── helpers ───────────────────────────────────────────────────────────────────

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

// ── Put / Get ─────────────────────────────────────────────────────────────────

func TestStore_PutAndGet(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		owner string
	}{
		{name: "basic env", id: "aaaa-bbbb-cccc-dddd", owner: "alice"},
		{name: "special chars in owner", id: "eeee-ffff-0000-1111", owner: "bob@example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			env := newEnv(tc.id, tc.owner)

			if err := store.Put(env); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := store.Get(env.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.ID != env.ID {
				t.Errorf("ID=%q, want %q", got.ID, env.ID)
			}
			if got.Owner != env.Owner {
				t.Errorf("Owner=%q, want %q", got.Owner, env.Owner)
			}
		})
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Get("does-not-exist")
	if err == nil {
		t.Fatal("expected ErrNotFound, got nil")
	}
}

// ── List with status filter ───────────────────────────────────────────────────

func TestStore_List(t *testing.T) {
	// Seed a fixed set of environments with mixed statuses.
	seed := []struct {
		id     string
		status string
	}{
		{"id-1", environment.StatusProvisioning},
		{"id-2", environment.StatusReady},
		{"id-3", environment.StatusFailed},
		{"id-4", environment.StatusReady},
	}

	cases := []struct {
		name      string
		filter    string
		wantIDs   []string
		wantCount int
	}{
		{
			name:      "no filter returns all",
			filter:    "",
			wantCount: 4,
		},
		{
			name:      "filter by ready",
			filter:    environment.StatusReady,
			wantCount: 2,
			wantIDs:   []string{"id-2", "id-4"},
		},
		{
			name:      "filter by provisioning",
			filter:    environment.StatusProvisioning,
			wantCount: 1,
			wantIDs:   []string{"id-1"},
		},
		{
			name:      "filter by failed",
			filter:    environment.StatusFailed,
			wantCount: 1,
			wantIDs:   []string{"id-3"},
		},
		{
			name:      "filter by destroyed returns empty",
			filter:    environment.StatusDestroyed,
			wantCount: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			for _, s := range seed {
				e := newEnv(s.id, "user")
				e.Status = s.status
				if err := store.Put(e); err != nil {
					t.Fatalf("Put %s: %v", s.id, err)
				}
			}

			got, err := store.List(tc.filter)
			if err != nil {
				t.Fatalf("List(%q): %v", tc.filter, err)
			}
			if len(got) != tc.wantCount {
				t.Errorf("got %d envs, want %d", len(got), tc.wantCount)
			}
			if len(tc.wantIDs) > 0 {
				gotIDs := make(map[string]bool, len(got))
				for _, e := range got {
					gotIDs[e.ID] = true
				}
				for _, id := range tc.wantIDs {
					if !gotIDs[id] {
						t.Errorf("expected env %q in results", id)
					}
				}
			}
		})
	}
}

// ── Delete ────────────────────────────────────────────────────────────────────

func TestStore_Delete(t *testing.T) {
	cases := []struct {
		name    string
		seedID  string
		delID   string
		wantErr bool // true → Delete should return an error
	}{
		{
			name:   "delete existing env",
			seedID: "del-id",
			delID:  "del-id",
		},
		// Deleting a non-existent ID is not an error in BoltDB (no-op).
		{
			name:   "delete non-existent env is silent",
			seedID: "",
			delID:  "ghost-id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			if tc.seedID != "" {
				if err := store.Put(newEnv(tc.seedID, "alice")); err != nil {
					t.Fatalf("Put: %v", err)
				}
			}

			err := store.Delete(tc.delID)
			if tc.wantErr && err == nil {
				t.Fatal("expected error from Delete, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.seedID != "" && tc.seedID == tc.delID {
				if _, err := store.Get(tc.delID); err == nil {
					t.Fatal("expected ErrNotFound after Delete, got nil")
				}
			}
		})
	}
}

// ── Put updates timestamp ─────────────────────────────────────────────────────

func TestStore_Put_UpdatesFields(t *testing.T) {
	cases := []struct {
		name          string
		initialStatus string
		updatedStatus string
	}{
		{name: "provisioning → ready", initialStatus: environment.StatusProvisioning, updatedStatus: environment.StatusReady},
		{name: "ready → failed", initialStatus: environment.StatusReady, updatedStatus: environment.StatusFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			env := newEnv("ts-id", "alice")
			env.Status = tc.initialStatus
			if err := store.Put(env); err != nil {
				t.Fatalf("first Put: %v", err)
			}
			first, _ := store.Get(env.ID)

			env.Status = tc.updatedStatus
			if err := store.Put(env); err != nil {
				t.Fatalf("second Put: %v", err)
			}
			second, _ := store.Get(env.ID)

			if second.Status != tc.updatedStatus {
				t.Errorf("Status=%q, want %q", second.Status, tc.updatedStatus)
			}
			// UpdatedAt must not regress.
			if second.UpdatedAt.Before(first.UpdatedAt) {
				t.Errorf("UpdatedAt regressed: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
			}
		})
	}
}

// ── persistence across open/close ────────────────────────────────────────────

func TestStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persistent.db")

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
		t.Errorf("VPCID=%q, want %q", got.VPCID, "vpc-123")
	}
}

// ── file creation ─────────────────────────────────────────────────────────────

func TestStore_CreatesFileOnOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "new.db")

	if _, err := os.Stat(dbPath); err == nil {
		t.Fatal("db file should not exist before Open")
	}

	store, err := environment.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close() //nolint:errcheck

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db file not created after Open: %v", err)
	}
}
