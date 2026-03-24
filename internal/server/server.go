package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ngaddam369/env-forge/internal/adminclient"
	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/steps"
	"github.com/rs/zerolog"
)

// stepPayload is the JSON body saga-conductor sends to every step endpoint.
type stepPayload struct {
	EnvID string `json:"env_id"`
}

// Server is the HTTP server that exposes step endpoints to saga-conductor
// and admin proxy endpoints for svid-exchange policy/token management.
// Each step is reachable at POST /steps/{name} (forward) and
// POST /steps/{name}/compensate (compensation).
type Server struct {
	store    environment.StateClient
	mux      *http.ServeMux
	log      zerolog.Logger
	allSteps map[string]steps.Step
	admin    *adminclient.Client // nil when admin gRPC not configured
}

// New creates a Server and registers all step, health, and admin proxy routes.
// ac may be nil when the svid-exchange admin address is not configured.
func New(store environment.StateClient, allSteps []steps.Step, ac *adminclient.Client, log zerolog.Logger) *Server {
	s := &Server{
		store:    store,
		mux:      http.NewServeMux(),
		log:      log,
		allSteps: make(map[string]steps.Step, len(allSteps)),
		admin:    ac,
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
	// Admin proxy endpoints — forward to svid-exchange admin gRPC via SPIFFE mTLS.
	s.mux.HandleFunc("GET /admin/policies", s.handleListPolicies)
	s.mux.HandleFunc("POST /admin/policies/reload", s.handleReloadPolicies)
	s.mux.HandleFunc("POST /admin/tokens/revoke", s.handleRevokeToken)
	s.mux.HandleFunc("GET /admin/tokens/revoked", s.handleListRevokedTokens)
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

// ── admin proxy handlers ─────────────────────────────────────────────────────

func (s *Server) adminUnavailable(w http.ResponseWriter) {
	http.Error(w, "svid-exchange admin not configured (set SVIDEXCHANGE_ADMIN_ADDR)", http.StatusServiceUnavailable)
}

// handleListPolicies proxies GET /admin/policies → svid-exchange ListPolicies.
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		s.adminUnavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	policies, err := s.admin.ListPolicies(ctx)
	if err != nil {
		s.log.Error().Err(err).Msg("list policies")
		http.Error(w, "list policies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if encErr := json.NewEncoder(w).Encode(map[string]any{"policies": policies}); encErr != nil {
		s.log.Error().Err(encErr).Msg("encode list policies response")
	}
}

// handleReloadPolicies proxies POST /admin/policies/reload → svid-exchange ReloadPolicy.
func (s *Server) handleReloadPolicies(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		s.adminUnavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.admin.ReloadPolicy(ctx); err != nil {
		s.log.Error().Err(err).Msg("reload policies")
		http.Error(w, "reload policies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleRevokeToken proxies POST /admin/tokens/revoke → svid-exchange RevokeToken.
// Body: {"token_id":"<jti>","expires_at":<unix>}
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		s.adminUnavailable(w)
		return
	}
	var req struct {
		TokenID   string `json:"token_id"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.TokenID == "" {
		http.Error(w, "token_id is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.admin.RevokeToken(ctx, req.TokenID, req.ExpiresAt); err != nil {
		s.log.Error().Err(err).Msg("revoke token")
		http.Error(w, "revoke token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleListRevokedTokens proxies GET /admin/tokens/revoked → svid-exchange ListRevokedTokens.
func (s *Server) handleListRevokedTokens(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		s.adminUnavailable(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tokens, err := s.admin.ListRevokedTokens(ctx)
	if err != nil {
		s.log.Error().Err(err).Msg("list revoked tokens")
		http.Error(w, "list revoked tokens: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if encErr := json.NewEncoder(w).Encode(map[string]any{"tokens": tokens}); encErr != nil {
		s.log.Error().Err(encErr).Msg("encode revoked tokens response")
	}
}
