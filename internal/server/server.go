package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/steps"
	"github.com/rs/zerolog"
)

// stepPayload is the JSON body saga-conductor sends to every step endpoint.
type stepPayload struct {
	EnvID string `json:"env_id"`
}

// Provisioner starts a provisioning saga for a new environment.
// The implementation is expected to block until the saga reaches a terminal state.
type Provisioner interface {
	Provision(ctx context.Context, env *environment.Environment) error
}

// Server is the HTTP server that exposes step endpoints to saga-conductor.
// Each step is reachable at POST /steps/{name} (forward) and
// POST /steps/{name}/compensate (compensation).
//
// It also exposes an admin API for environment management:
//   - POST /envs/create — provision a new environment (async)
//   - GET  /envs        — list all environments
//   - GET  /envs/{id}   — get a single environment by ID prefix
type Server struct {
	store       *environment.Store
	mux         *http.ServeMux
	log         zerolog.Logger
	allSteps    map[string]steps.Step
	provisioner Provisioner // nil when admin API is disabled
}

// New creates a Server and registers all step routes.
// Pass a non-nil provisioner to enable the POST /envs/create admin endpoint.
func New(store *environment.Store, allSteps []steps.Step, provisioner Provisioner, log zerolog.Logger) *Server {
	s := &Server{
		store:       store,
		mux:         http.NewServeMux(),
		log:         log,
		allSteps:    make(map[string]steps.Step, len(allSteps)),
		provisioner: provisioner,
	}
	for _, step := range allSteps {
		s.allSteps[step.Name()] = step
		name := step.Name()
		s.mux.HandleFunc("POST /steps/"+name, s.handleForward(name))
		s.mux.HandleFunc("POST /steps/"+name+"/compensate", s.handleCompensate(name))
	}
	s.mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.HandleFunc("POST /envs/create", s.handleCreateEnv)
	s.mux.HandleFunc("GET /envs", s.handleListEnvs)
	s.mux.HandleFunc("GET /envs/{id}", s.handleGetEnv)
	return s
}

// ServeHTTP implements http.Handler, delegating to the internal ServeMux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleForward(stepName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.handle(w, r, stepName, false)
	}
}

func (s *Server) handleCompensate(stepName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.handle(w, r, stepName, true)
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, stepName string, compensate bool) {
	log := s.log.With().Str("step", stepName).Bool("compensate", compensate).Logger()

	var p stepPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		log.Error().Err(err).Msg("decode payload")
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if p.EnvID == "" {
		http.Error(w, "missing env_id", http.StatusBadRequest)
		return
	}

	env, err := s.store.Get(p.EnvID)
	if err != nil {
		if errors.Is(err, environment.ErrNotFound) {
			http.Error(w, fmt.Sprintf("environment %s not found", p.EnvID), http.StatusNotFound)
			return
		}
		log.Error().Err(err).Msg("load environment")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	step, ok := s.allSteps[stepName]
	if !ok {
		http.Error(w, "unknown step: "+stepName, http.StatusNotFound)
		return
	}

	ctx := r.Context()

	if !compensate {
		// Idempotency check: if already done, return 200 without re-executing.
		done, err := step.IsAlreadyDone(ctx, env)
		if err != nil {
			log.Error().Err(err).Msg("is_already_done check failed")
			http.Error(w, "is_already_done error", http.StatusInternalServerError)
			return
		}
		if done {
			log.Info().Msg("step already done — skipping (idempotent)")
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := step.Execute(ctx, env, s.store); err != nil {
			log.Error().Err(err).Msg("step execute failed")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Info().Msg("step executed successfully")
	} else {
		if err := step.Compensate(ctx, env, s.store); err != nil {
			log.Error().Err(err).Msg("step compensate failed")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Info().Msg("step compensated successfully")
	}

	w.WriteHeader(http.StatusOK)
}

// handleCreateEnv handles POST /envs/create.
// It creates a new environment and starts its provisioning saga asynchronously.
func (s *Server) handleCreateEnv(w http.ResponseWriter, r *http.Request) {
	if s.provisioner == nil {
		http.Error(w, "provisioner not configured", http.StatusServiceUnavailable)
		return
	}

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

// handleGetEnv handles GET /envs/{id}.
// The {id} path value is matched as a prefix against stored environment IDs.
func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	prefix := r.PathValue("id")
	envs, err := s.store.List("")
	if err != nil {
		s.log.Error().Err(err).Msg("list environments for prefix match")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, e := range envs {
		if len(e.ID) >= len(prefix) && e.ID[:len(prefix)] == prefix {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(e); err != nil {
				s.log.Error().Err(err).Msg("encode env response")
			}
			return
		}
	}
	http.Error(w, fmt.Sprintf("environment %q not found", prefix), http.StatusNotFound)
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
