package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/steps"
	"github.com/rs/zerolog"
)

// stepPayload is the JSON body saga-conductor sends to every step endpoint.
type stepPayload struct {
	EnvID string `json:"env_id"`
}

// Server is the HTTP server that exposes step endpoints to saga-conductor.
// Each step is reachable at POST /steps/{name} (forward) and
// POST /steps/{name}/compensate (compensation).
type Server struct {
	store    environment.StateClient
	mux      *http.ServeMux
	log      zerolog.Logger
	allSteps map[string]steps.Step
}

// New creates a Server and registers all step and health routes.
func New(store environment.StateClient, allSteps []steps.Step, log zerolog.Logger) *Server {
	s := &Server{
		store:    store,
		mux:      http.NewServeMux(),
		log:      log,
		allSteps: make(map[string]steps.Step, len(allSteps)),
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

// ListenAndServe starts the HTTP server on addr (e.g. ":9091").
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
