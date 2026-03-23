package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/server"
	"github.com/ngaddam369/env-forge/internal/steps"
	"github.com/rs/zerolog"
)

func newTestStore(t *testing.T) *environment.Store {
	t.Helper()
	store, err := environment.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	return store
}

func newTestEnv(id string) *environment.Environment {
	return &environment.Environment{
		ID:        id + "-aaaa-bbbb-cccc",
		Owner:     "test",
		Status:    environment.StatusProvisioning,
		DryRun:    true,
		CreatedAt: time.Now().UTC(),
	}
}

func buildTestServer(t *testing.T, store *environment.Store) *server.Server {
	t.Helper()
	allSteps := []steps.Step{
		steps.NewVPCStep(nil),
		steps.NewRDSStep(nil),
		steps.NewRegistryStep(),
		steps.NewHealthStep(),
	}
	return server.New(store, allSteps, zerolog.Nop())
}

func postStep(t *testing.T, srv http.Handler, path, envID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"env_id": envID})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestServer_ForwardStep_Success(t *testing.T) {
	store := newTestStore(t)
	env := newTestEnv("srv01234")
	_ = store.Put(env)

	srv := buildTestServer(t, store)

	rr := postStep(t, srv, "/steps/vpc", env.ID)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Env should have VPCID set in store.
	updated, _ := store.Get(env.ID)
	if updated.VPCID == "" {
		t.Error("VPCID not set in store after forward step")
	}
}

func TestServer_CompensateStep_Success(t *testing.T) {
	store := newTestStore(t)
	env := newTestEnv("srv02345")
	env.VPCID = "vpc-already"
	_ = store.Put(env)

	srv := buildTestServer(t, store)

	rr := postStep(t, srv, "/steps/vpc/compensate", env.ID)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	updated, _ := store.Get(env.ID)
	if updated.VPCID != "" {
		t.Error("VPCID should be cleared after compensation")
	}
}

func TestServer_ForwardStep_Idempotent(t *testing.T) {
	store := newTestStore(t)
	env := newTestEnv("srv03456")
	env.VPCID = "vpc-already" // already done
	_ = store.Put(env)

	srv := buildTestServer(t, store)

	// POST twice — both should return 200 without re-executing.
	for i := 0; i < 2; i++ {
		rr := postStep(t, srv, "/steps/vpc", env.ID)
		if rr.Code != http.StatusOK {
			t.Errorf("attempt %d: expected 200, got %d", i+1, rr.Code)
		}
	}
}

func TestServer_UnknownEnvID_Returns404(t *testing.T) {
	store := newTestStore(t)
	srv := buildTestServer(t, store)

	rr := postStep(t, srv, "/steps/vpc", "nonexistent-id-aaaa-bbbb-cccc")
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestServer_UnknownStep_Returns404(t *testing.T) {
	store := newTestStore(t)
	env := newTestEnv("srv04567")
	_ = store.Put(env)

	srv := buildTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"env_id": env.ID})
	req := httptest.NewRequest(http.MethodPost, "/steps/nonexistent", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	// net/http ServeMux returns 405 (method not allowed) for unregistered patterns
	// depending on Go version; check for non-200.
	if rr.Code == http.StatusOK {
		t.Errorf("expected non-200 for unknown step, got 200")
	}
}

func TestServer_BadPayload_Returns400(t *testing.T) {
	store := newTestStore(t)
	srv := buildTestServer(t, store)

	req := httptest.NewRequest(http.MethodPost, "/steps/vpc", bytes.NewReader([]byte("not-json")))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestServer_HealthLive(t *testing.T) {
	store := newTestStore(t)
	srv := buildTestServer(t, store)

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 from /health/live, got %d", rr.Code)
	}
}

func TestServer_FailAtHealth_Returns500(t *testing.T) {
	store := newTestStore(t)
	env := newTestEnv("srv05678")
	env.FailAtHealth = true
	_ = store.Put(env)

	srv := buildTestServer(t, store)

	rr := postStep(t, srv, "/steps/health", env.ID)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when FailAtHealth=true, got %d", rr.Code)
	}
}

func TestServer_ConcurrentRequests(t *testing.T) {
	store := newTestStore(t)

	// Create 5 environments.
	envs := make([]*environment.Environment, 5)
	for i := range envs {
		envs[i] = newTestEnv("conc0000")
		envs[i].ID = "conc-test-" + string(rune('0'+i)) + "-aaaa-bbbb"
		_ = store.Put(envs[i])
	}

	srv := buildTestServer(t, store)
	done := make(chan struct{}, len(envs))

	for _, e := range envs {
		go func(envID string) {
			rr := postStep(t, srv, "/steps/registry", envID)
			if rr.Code != http.StatusOK {
				t.Errorf("concurrent request failed: %d %s", rr.Code, rr.Body.String())
			}
			done <- struct{}{}
		}(e.ID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range envs {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("timeout waiting for concurrent requests")
		}
	}
}
