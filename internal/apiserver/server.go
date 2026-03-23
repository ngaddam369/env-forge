package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/rs/zerolog"
)

// Provisioner starts a provisioning saga for a new environment.
// The implementation is expected to block until the saga reaches a terminal state.
type Provisioner interface {
	Provision(ctx context.Context, env *environment.Environment) error
}

// Verifier validates a Bearer JWT.
// A nil Verifier disables JWT validation (dev/local mode).
type Verifier interface {
	Verify(token, audience string) error
}

// Server is the HTTP API server for forge-api.
//
// User-facing routes:
//   - POST /envs/create  — provision a new environment (async)
//   - GET  /envs         — list all environments
//   - GET  /envs/{id}    — get a single environment by ID prefix
//
// Internal routes (JWT-protected when verifier != nil):
//   - GET /internal/envs/{id} — read env state (called by forge-worker)
//   - PUT /internal/envs/{id} — write env state (called by forge-worker)
type Server struct {
	store       *environment.Store
	mux         *http.ServeMux
	log         zerolog.Logger
	provisioner Provisioner
	verifier    Verifier // nil → skip JWT validation
	audience    string   // expected JWT audience (SPIFFE ID of forge-api)
}

// New creates a Server and registers all routes.
// Pass nil verifier to disable JWT validation on internal endpoints.
// audience is the expected JWT aud claim (e.g. "spiffe://cluster.local/ns/default/sa/forge-api").
func New(store *environment.Store, provisioner Provisioner, verifier Verifier, audience string, log zerolog.Logger) *Server {
	s := &Server{
		store:       store,
		mux:         http.NewServeMux(),
		log:         log,
		provisioner: provisioner,
		verifier:    verifier,
		audience:    audience,
	}
	s.mux.HandleFunc("POST /envs/create", s.handleCreateEnv)
	s.mux.HandleFunc("GET /envs", s.handleListEnvs)
	s.mux.HandleFunc("GET /envs/{id}", s.handleGetEnv)
	s.mux.HandleFunc("GET /internal/envs/{id}", s.requireAuth(s.handleInternalGetEnv))
	s.mux.HandleFunc("PUT /internal/envs/{id}", s.requireAuth(s.handleInternalPutEnv))
	s.mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// requireAuth is middleware that validates the Bearer JWT when a verifier is configured.
// When the verifier is nil, the request passes through immediately (dev mode).
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.verifier != nil {
			authHeader := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(authHeader, "Bearer ")
			if !ok || token == "" {
				http.Error(w, "missing Bearer token", http.StatusUnauthorized)
				return
			}
			if err := s.verifier.Verify(token, s.audience); err != nil {
				s.log.Warn().Err(err).Msg("JWT verification failed")
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
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

// handleInternalGetEnv handles GET /internal/envs/{id} — used by forge-worker.
func (s *Server) handleInternalGetEnv(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) handleInternalPutEnv(w http.ResponseWriter, r *http.Request) {
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
