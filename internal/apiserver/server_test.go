package apiserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ngaddam369/env-forge/internal/apiserver"
	"github.com/ngaddam369/env-forge/internal/environment"
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

type noopProvisioner struct{}

func (noopProvisioner) Provision(_ context.Context, _ *environment.Environment) error { return nil }

// slowProvisioner blocks until its context is cancelled.
type slowProvisioner struct{ started chan struct{} }

func (p *slowProvisioner) Provision(ctx context.Context, _ *environment.Environment) error {
	close(p.started)
	<-ctx.Done()
	return ctx.Err()
}

func newServer(t *testing.T, store *environment.Store, prov apiserver.Provisioner) *apiserver.Server {
	t.Helper()
	return apiserver.New(store, prov, nil, "", nil, zerolog.Nop())
}

func seedEnv(t *testing.T, store *environment.Store, id, owner string, status string) *environment.Environment {
	t.Helper()
	env := &environment.Environment{
		ID:        id,
		Owner:     owner,
		Status:    status,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Put(env); err != nil {
		t.Fatalf("seed env: %v", err)
	}
	return env
}

// ── POST /envs/create ────────────────────────────────────────────────────────

func TestHandleCreateEnv(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantEnvID  bool // true → response must contain a non-empty env_id
	}{
		{
			name:       "valid request accepted",
			body:       `{"owner":"alice","dry_run":true}`,
			wantStatus: http.StatusAccepted,
			wantEnvID:  true,
		},
		{
			name:       "missing owner returns 400",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid JSON returns 400",
			body:       `not json`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty body returns 400",
			body:       ``,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			srv := newServer(t, store, noopProvisioner{})

			req := httptest.NewRequest(http.MethodPost, "/envs/create", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantEnvID {
				var resp map[string]string
				if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if resp["env_id"] == "" {
					t.Error("expected non-empty env_id in response")
				}
			}
		})
	}
}

// ── GET /envs ────────────────────────────────────────────────────────────────

func TestHandleListEnvs(t *testing.T) {
	cases := []struct {
		name      string
		seedCount int
		query     string
		wantCount int
	}{
		{
			name:      "empty store returns empty array",
			wantCount: 0,
		},
		{
			name:      "returns all seeded environments",
			seedCount: 3,
			wantCount: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			for i := range tc.seedCount {
				seedEnv(t, store,
					strings.Repeat("a", 8)+string(rune('0'+i))+"-0000-0000-0000-000000000001",
					"user", environment.StatusReady)
			}

			srv := newServer(t, store, noopProvisioner{})
			req := httptest.NewRequest(http.MethodGet, "/envs"+tc.query, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type=%q, want application/json", ct)
			}
			var envs []*environment.Environment
			if err := json.NewDecoder(rr.Body).Decode(&envs); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(envs) != tc.wantCount {
				t.Errorf("got %d environments, want %d", len(envs), tc.wantCount)
			}
		})
	}
}

// ── GET /envs/{id} ───────────────────────────────────────────────────────────

func TestHandleGetEnv(t *testing.T) {
	cases := []struct {
		name       string
		prefix     string
		seed       bool // whether to seed the env before the request
		wantStatus int
	}{
		{
			name:       "prefix match returns 200",
			prefix:     "deaddead",
			seed:       true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "unknown prefix returns 404",
			prefix:     "nosuchid",
			seed:       false,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			var env *environment.Environment
			if tc.seed {
				env = seedEnv(t, store, "deaddead-0000-0000-0000-000000000001", "dave", environment.StatusProvisioning)
			}

			srv := newServer(t, store, noopProvisioner{})
			req := httptest.NewRequest(http.MethodGet, "/envs/"+tc.prefix, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				var got environment.Environment
				if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.ID != env.ID {
					t.Errorf("ID=%q, want %q", got.ID, env.ID)
				}
			}
		})
	}
}

// ── health endpoints ──────────────────────────────────────────────────────────

func TestHealthEndpoints(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		closeStore  bool // simulate an unhealthy store
		wantStatus  int
	}{
		{name: "live returns 200", path: "/health/live", wantStatus: http.StatusOK},
		{name: "ready returns 200 when store is accessible", path: "/health/ready", wantStatus: http.StatusOK},
		{name: "ready returns 503 when store is closed", path: "/health/ready", closeStore: true, wantStatus: http.StatusServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			if tc.closeStore {
				if err := store.Close(); err != nil {
					t.Fatalf("close store: %v", err)
				}
			}

			srv := newServer(t, store, noopProvisioner{})
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d", rr.Code, tc.wantStatus)
			}
		})
	}
}

// ── internal endpoints (dev mode — no JWT) ────────────────────────────────────

func TestInternalEndpoints(t *testing.T) {
	const envID = "cafecafe-0000-0000-0000-000000000001"

	cases := []struct {
		name       string
		method     string
		path       string
		buildBody  func(env *environment.Environment) []byte
		wantStatus int
		checkStore func(t *testing.T, store *environment.Store)
	}{
		{
			name:       "GET internal env returns 200",
			method:     http.MethodGet,
			path:       "/internal/envs/" + envID,
			wantStatus: http.StatusOK,
		},
		{
			name:   "PUT internal env updates store",
			method: http.MethodPut,
			path:   "/internal/envs/" + envID,
			buildBody: func(env *environment.Environment) []byte {
				env.Status = environment.StatusReady
				data, _ := json.Marshal(env)
				return data
			},
			wantStatus: http.StatusOK,
			checkStore: func(t *testing.T, store *environment.Store) {
				t.Helper()
				got, err := store.Get(envID)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if got.Status != environment.StatusReady {
					t.Errorf("status=%q, want %q", got.Status, environment.StatusReady)
				}
			},
		},
		{
			name:       "GET internal env not found returns 404",
			method:     http.MethodGet,
			path:       "/internal/envs/does-not-exist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "PUT internal env with bad JSON returns 400",
			method:     http.MethodPut,
			path:       "/internal/envs/" + envID,
			buildBody:  func(_ *environment.Environment) []byte { return []byte("not json") },
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			env := seedEnv(t, store, envID, "system", environment.StatusProvisioning)

			var body []byte
			if tc.buildBody != nil {
				body = tc.buildBody(env)
			}

			srv := newServer(t, store, noopProvisioner{})
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(body))
			if len(body) > 0 {
				req.Header.Set("Content-Type", "application/json")
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.checkStore != nil {
				tc.checkStore(t, store)
			}
		})
	}
}

// ── shutdown drains in-flight provisions ──────────────────────────────────────

// TestHandleCreateEnv_ContextCancelledOnShutdown verifies that the server
// lifecycle context (not context.Background()) is passed to Provision goroutines
// so they are cancelled on shutdown, and ListenAndServe waits for them to finish.
func TestHandleCreateEnv_ContextCancelledOnShutdown(t *testing.T) {
	store := newTestStore(t)
	prov := &slowProvisioner{started: make(chan struct{})}
	srv := newServer(t, store, prov)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.ListenAndServe(ctx, "127.0.0.1:0")
	}()
	time.Sleep(50 * time.Millisecond) // let ListenAndServe set srvCtx

	body := `{"owner":"bob","dry_run":true}`
	req := httptest.NewRequest(http.MethodPost, "/envs/create", strings.NewReader(body))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rr.Code)
	}

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provisioner goroutine did not start")
	}

	cancel()

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after context cancel")
	}
}
