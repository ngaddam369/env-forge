package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	svidclient "github.com/ngaddam369/svid-exchange/pkg/client"
	"github.com/rs/zerolog"

	"github.com/ngaddam369/env-forge/internal/environment"
	pb "github.com/ngaddam369/saga-conductor/proto/saga/v1"
)

// Provisioner starts a provisioning saga for a new environment.
// The implementation is expected to block until the saga reaches a terminal state.
type Provisioner interface {
	Provision(ctx context.Context, env *environment.Environment) error
}

// SagaClient proxies saga operations to saga-conductor.
// A nil SagaClient disables the /sagas and /envs/{id}/saga proxy endpoints.
type SagaClient interface {
	GetSaga(ctx context.Context, sagaID string) (*pb.SagaExecution, error)
	ListSagas(ctx context.Context, req *pb.ListSagasRequest) ([]*pb.SagaExecution, string, error)
	AbortSaga(ctx context.Context, sagaID string) (*pb.SagaExecution, error)
}

// Server is the HTTP API server for forge-api.
//
// User-facing routes:
//   - POST /envs/create         — provision a new environment (async)
//   - GET  /envs                — list all environments
//   - GET  /envs/{id}           — get a single environment by ID prefix
//   - POST /envs/{id}/abort     — abort the running saga for an environment
//   - GET  /envs/{id}/saga      — get saga step detail for an environment
//   - GET  /sagas               — list sagas (?status=X, ?page_size=N, ?cursor=C)
//
// Internal routes (JWT-protected when verifier != nil):
//   - GET /internal/envs/{id}  — read env state (called by forge-worker)
//   - PUT /internal/envs/{id}  — write env state (called by forge-worker)
type Server struct {
	store       *environment.Store
	mux         *http.ServeMux
	log         zerolog.Logger
	provisioner Provisioner
	verifier    *svidclient.Verifier // nil → dev mode, skip JWT validation
	audience    string               // expected JWT aud claim (SPIFFE ID of forge-api)
	saga        SagaClient           // nil → saga proxy endpoints return 503
}

// New creates a Server and registers all routes.
//
//   - verifier: *svidclient.Verifier for JWT validation; nil disables auth (dev mode)
//   - audience: expected aud claim, e.g. "spiffe://cluster.local/ns/default/sa/forge-api"
//   - sagaClient: optional SagaClient for saga proxy endpoints; nil disables them
func New(store *environment.Store, provisioner Provisioner, verifier *svidclient.Verifier, audience string, sagaClient SagaClient, log zerolog.Logger) *Server {
	s := &Server{
		store:       store,
		mux:         http.NewServeMux(),
		log:         log,
		provisioner: provisioner,
		verifier:    verifier,
		audience:    audience,
		saga:        sagaClient,
	}

	// Public routes
	s.mux.HandleFunc("POST /envs/create", s.handleCreateEnv)
	s.mux.HandleFunc("GET /envs", s.handleListEnvs)
	s.mux.HandleFunc("GET /envs/{id}", s.handleGetEnv)
	s.mux.HandleFunc("POST /envs/{id}/abort", s.handleAbortEnv)
	s.mux.HandleFunc("GET /envs/{id}/saga", s.handleGetEnvSaga)
	s.mux.HandleFunc("GET /sagas", s.handleListSagas)

	// Internal routes — JWT-protected via svid-exchange NewMiddleware.
	// In dev mode (verifier == nil) the middleware passes through immediately.
	s.mux.Handle("GET /internal/envs/{id}", s.authMiddleware(http.HandlerFunc(s.handleInternalGetEnv)))
	s.mux.Handle("PUT /internal/envs/{id}", s.authMiddleware(http.HandlerFunc(s.handleInternalPutEnv)))

	s.mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// authMiddleware wraps a handler with JWT validation when a verifier is set.
// In dev mode (verifier == nil) it passes through with no authentication.
// Uses svid-exchange NewMiddleware so that JWT claims are stored in context
// and accessible via svidclient.ClaimsFromContext.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.verifier == nil {
		return next
	}
	return svidclient.NewMiddleware(s.verifier, s.audience, next)
}

// handleCreateEnv handles POST /envs/create.
func (s *Server) handleCreateEnv(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner        string `json:"owner"`
		DryRun       bool   `json:"dry_run"`
		FailAtHealth bool   `json:"fail_at_health"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Owner == "" {
		http.Error(w, "owner is required", http.StatusBadRequest)
		return
	}

	env := &environment.Environment{
		ID:           uuid.New().String(),
		Owner:        req.Owner,
		Status:       environment.StatusProvisioning,
		DryRun:       req.DryRun,
		FailAtHealth: req.FailAtHealth,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.store.Put(env); err != nil {
		s.log.Error().Err(err).Msg("save environment")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	go func() {
		if err := s.provisioner.Provision(context.Background(), env); err != nil {
			s.log.Error().Err(err).Str("env_id", env.ID).Msg("provisioning failed")
			env.Status = environment.StatusFailed
			if putErr := s.store.Put(env); putErr != nil {
				s.log.Error().Err(putErr).Msg("update env status to failed")
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(map[string]string{"env_id": env.ID}); err != nil {
		s.log.Error().Err(err).Msg("encode create response")
	}
}

// handleListEnvs handles GET /envs.
func (s *Server) handleListEnvs(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	envs, err := s.store.List(statusFilter)
	if err != nil {
		s.log.Error().Err(err).Msg("list environments")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(envs); err != nil {
		s.log.Error().Err(err).Msg("encode list response")
	}
}

// handleGetEnv handles GET /envs/{id} — prefix match.
func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("id")
	env, err := s.findByPrefix(prefix)
	if err != nil {
		if errors.Is(err, environment.ErrNotFound) {
			http.Error(w, fmt.Sprintf("environment %q not found", prefix), http.StatusNotFound)
			return
		}
		s.log.Error().Err(err).Msg("get environment")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(env); err != nil {
		s.log.Error().Err(err).Msg("encode env response")
	}
}

// handleAbortEnv handles POST /envs/{id}/abort.
// Looks up the environment's SagaID and calls conductor AbortSaga.
func (s *Server) handleAbortEnv(w http.ResponseWriter, r *http.Request) {
	if s.saga == nil {
		http.Error(w, "saga client not configured", http.StatusServiceUnavailable)
		return
	}
	prefix := r.PathValue("id")
	env, err := s.findByPrefix(prefix)
	if err != nil {
		if errors.Is(err, environment.ErrNotFound) {
			http.Error(w, fmt.Sprintf("environment %q not found", prefix), http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if env.SagaID == "" {
		http.Error(w, "environment has no associated saga", http.StatusBadRequest)
		return
	}
	exec, err := s.saga.AbortSaga(r.Context(), env.SagaID)
	if err != nil {
		s.log.Error().Err(err).Str("saga_id", env.SagaID).Msg("abort saga")
		http.Error(w, "abort failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(exec); err != nil {
		s.log.Error().Err(err).Msg("encode abort response")
	}
}

// handleGetEnvSaga handles GET /envs/{id}/saga.
// Returns the full saga execution (step-level detail) for an environment.
func (s *Server) handleGetEnvSaga(w http.ResponseWriter, r *http.Request) {
	if s.saga == nil {
		http.Error(w, "saga client not configured", http.StatusServiceUnavailable)
		return
	}
	prefix := r.PathValue("id")
	env, err := s.findByPrefix(prefix)
	if err != nil {
		if errors.Is(err, environment.ErrNotFound) {
			http.Error(w, fmt.Sprintf("environment %q not found", prefix), http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if env.SagaID == "" {
		http.Error(w, "environment has no associated saga", http.StatusBadRequest)
		return
	}
	exec, err := s.saga.GetSaga(r.Context(), env.SagaID)
	if err != nil {
		s.log.Error().Err(err).Str("saga_id", env.SagaID).Msg("get saga")
		http.Error(w, "get saga failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(exec); err != nil {
		s.log.Error().Err(err).Msg("encode saga response")
	}
}

// handleListSagas handles GET /sagas — proxies to saga-conductor ListSagas.
// Query params: status (e.g. "RUNNING"), page_size (default 100), cursor (next-page token).
func (s *Server) handleListSagas(w http.ResponseWriter, r *http.Request) {
	if s.saga == nil {
		http.Error(w, "saga client not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	req := &pb.ListSagasRequest{
		PageToken: q.Get("cursor"),
	}
	if ps := q.Get("page_size"); ps != "" {
		var n int32
		if _, err := fmt.Sscanf(ps, "%d", &n); err == nil {
			req.PageSize = n
		}
	}
	if st := q.Get("status"); st != "" {
		if mapped, ok := pb.SagaStatus_value["SAGA_STATUS_"+st]; ok {
			req.Status = pb.SagaStatus(mapped)
		}
	}

	sagas, nextCursor, err := s.saga.ListSagas(r.Context(), req)
	if err != nil {
		s.log.Error().Err(err).Msg("list sagas")
		http.Error(w, "list sagas failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Sagas         []*pb.SagaExecution `json:"sagas"`
		NextPageToken string              `json:"next_page_token,omitempty"`
	}{Sagas: sagas, NextPageToken: nextCursor}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error().Err(err).Msg("encode sagas response")
	}
}

// handleInternalGetEnv handles GET /internal/envs/{id} — used by forge-worker.
// Requires env:read scope in the JWT (enforced by authMiddleware → scope check).
func (s *Server) handleInternalGetEnv(w http.ResponseWriter, r *http.Request) {
	// Check scope when JWT auth is active.
	if s.verifier != nil {
		claims, ok := svidclient.ClaimsFromContext(r.Context())
		if !ok || !svidclient.HasScope(claims, "env:read") {
			s.log.Warn().Msg("internal GET denied: missing env:read scope")
			http.Error(w, "forbidden: missing env:read scope", http.StatusForbidden)
			return
		}
		sub := fmt.Sprintf("%v", claims["sub"])
		scope := fmt.Sprintf("%v", claims["scope"])
		s.log.Info().Str("sub", sub).Str("scope", scope).Msg("internal GET authorized")
	}

	id := r.PathValue("id")
	env, err := s.store.Get(id)
	if err != nil {
		if errors.Is(err, environment.ErrNotFound) {
			http.Error(w, fmt.Sprintf("environment %s not found", id), http.StatusNotFound)
			return
		}
		s.log.Error().Err(err).Msg("get environment (internal)")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(env); err != nil {
		s.log.Error().Err(err).Msg("encode env (internal)")
	}
}

// handleInternalPutEnv handles PUT /internal/envs/{id} — used by forge-worker.
// Requires env:write scope in the JWT (enforced by authMiddleware → scope check).
func (s *Server) handleInternalPutEnv(w http.ResponseWriter, r *http.Request) {
	// Check scope when JWT auth is active.
	if s.verifier != nil {
		claims, ok := svidclient.ClaimsFromContext(r.Context())
		if !ok || !svidclient.HasAllScopes(claims, []string{"env:read", "env:write"}) {
			s.log.Warn().Msg("internal PUT denied: missing env:write scope")
			http.Error(w, "forbidden: missing env:write scope", http.StatusForbidden)
			return
		}
		sub := fmt.Sprintf("%v", claims["sub"])
		scope := fmt.Sprintf("%v", claims["scope"])
		s.log.Info().Str("sub", sub).Str("scope", scope).Msg("internal PUT authorized")
	}

	var env environment.Environment
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := s.store.Put(&env); err != nil {
		s.log.Error().Err(err).Msg("put environment (internal)")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// findByPrefix finds an environment whose ID starts with the given prefix.
func (s *Server) findByPrefix(prefix string) (*environment.Environment, error) {
	envs, err := s.store.List("")
	if err != nil {
		return nil, err
	}
	for _, e := range envs {
		if len(e.ID) >= len(prefix) && e.ID[:len(prefix)] == prefix {
			return e, nil
		}
	}
	return nil, environment.ErrNotFound
}

// ListenAndServe starts the HTTP server on addr (e.g. ":9090").
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s}
	go func() {
		<-ctx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			s.log.Error().Err(err).Msg("shutdown http server")
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
