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

// ── helpers ───────────────────────────────────────────────────────────────────

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
	return server.New(store, allSteps, nil, zerolog.Nop())
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

// ── step forward / compensate ─────────────────────────────────────────────────

func TestServer_StepExecution(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		setupEnv   func(*environment.Environment)
		wantStatus int
		checkStore func(t *testing.T, env *environment.Environment)
	}{
		{
			name:       "forward vpc sets VPCID",
			path:       "/steps/vpc",
			wantStatus: http.StatusOK,
			checkStore: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.VPCID == "" {
					t.Error("expected VPCID to be set in store after forward")
				}
			},
		},
		{
			name: "compensate vpc clears VPCID",
			path: "/steps/vpc/compensate",
			setupEnv: func(env *environment.Environment) {
				env.VPCID = "vpc-already"
			},
			wantStatus: http.StatusOK,
			checkStore: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.VPCID != "" {
					t.Errorf("expected VPCID cleared after compensate, got %q", env.VPCID)
				}
			},
		},
		{
			name: "forward vpc idempotent when already done",
			path: "/steps/vpc",
			setupEnv: func(env *environment.Environment) {
				env.VPCID = "vpc-already"
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "forward registry sets status=ready",
			path:       "/steps/registry",
			wantStatus: http.StatusOK,
			checkStore: func(t *testing.T, env *environment.Environment) {
				t.Helper()
				if env.Status != environment.StatusReady {
					t.Errorf("expected status=ready, got %s", env.Status)
				}
			},
		},
		{
			name:       "health step fails when FailAtHealth=true",
			path:       "/steps/health",
			setupEnv:   func(env *environment.Environment) { env.FailAtHealth = true },
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			env := newTestEnv("srvXXXX")
			if tc.setupEnv != nil {
				tc.setupEnv(env)
			}
			_ = store.Put(env)

			srv := buildTestServer(t, store)
			rr := postStep(t, srv, tc.path, env.ID)

			if rr.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.checkStore != nil {
				updated, err := store.Get(env.ID)
				if err != nil {
					t.Fatalf("get from store: %v", err)
				}
				tc.checkStore(t, updated)
			}
		})
	}
}

// TestServer_ForwardStep_Idempotent calls the same endpoint twice to confirm
// the second call is a no-op, not a re-execution.
func TestServer_ForwardStep_Idempotent(t *testing.T) {
	store := newTestStore(t)
	env := newTestEnv("srv03456")
	env.VPCID = "vpc-already"
	_ = store.Put(env)

	srv := buildTestServer(t, store)
	for i := range 2 {
		rr := postStep(t, srv, "/steps/vpc", env.ID)
		if rr.Code != http.StatusOK {
			t.Errorf("attempt %d: expected 200, got %d", i+1, rr.Code)
		}
	}
}

// ── error cases ───────────────────────────────────────────────────────────────

func TestServer_ErrorCases(t *testing.T) {
	cases := []struct {
		name       string
		buildReq   func(envID string) *http.Request
		wantStatus int
	}{
		{
			name: "unknown env ID returns 404",
			buildReq: func(_ string) *http.Request {
				body, _ := json.Marshal(map[string]string{"env_id": "nonexistent-id-aaaa-bbbb-cccc"})
				req := httptest.NewRequest(http.MethodPost, "/steps/vpc", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				return req
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "unknown step name returns non-200",
			buildReq: func(envID string) *http.Request {
				body, _ := json.Marshal(map[string]string{"env_id": envID})
				req := httptest.NewRequest(http.MethodPost, "/steps/nonexistent", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				return req
			},
			// ServeMux returns 405 for unregistered patterns in newer Go versions.
			wantStatus: -1, // any non-200
		},
		{
			name: "malformed JSON body returns 400",
			buildReq: func(_ string) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/steps/vpc", bytes.NewReader([]byte("not-json")))
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "missing env_id in body returns 400",
			buildReq: func(_ string) *http.Request {
				body, _ := json.Marshal(map[string]string{})
				req := httptest.NewRequest(http.MethodPost, "/steps/vpc", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				return req
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			env := newTestEnv("errXXXX")
			_ = store.Put(env)

			srv := buildTestServer(t, store)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, tc.buildReq(env.ID))

			if tc.wantStatus == -1 {
				if rr.Code == http.StatusOK {
					t.Errorf("expected non-200, got 200")
				}
			} else if rr.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// ── health endpoints ──────────────────────────────────────────────────────────

func TestServer_HealthEndpoints(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "live returns 200", path: "/health/live", wantStatus: http.StatusOK},
		{name: "ready returns 200", path: "/health/ready", wantStatus: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			srv := buildTestServer(t, store)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d", rr.Code, tc.wantStatus)
			}
		})
	}
}

// ── concurrency ───────────────────────────────────────────────────────────────

func TestServer_ConcurrentRequests(t *testing.T) {
	store := newTestStore(t)

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
