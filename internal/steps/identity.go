package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/ngaddam369/env-forge/internal/environment"
	adminv1 "github.com/ngaddam369/svid-exchange/proto/admin/v1"
	"google.golang.org/grpc"
)

// IdentityStep registers workload identities for the provisioned environment:
//  1. Calls the svid-exchange admin gRPC API to create an exchange policy
//     allowing the app identity to obtain tokens scoped to the db-proxy.
//  2. Stores SPIRE entry IDs (populated by SPIRE registration — see SPIRE
//     manifests in k8s/spire/ for workload entry configuration).
//
// In the minikube demo, SPIRE workload entries are registered via the SPIRE
// k8s workload attestor (entries defined in the SPIRE server config). This step
// registers the svid-exchange policy that lets those identities exchange tokens.
//
// Compensation deletes the svid-exchange policy.
type IdentityStep struct {
	svidConn    *grpc.ClientConn
	trustDomain string
}

// NewIdentityStep creates an IdentityStep.
// Pass nil for svidConn to use dry-run mode.
func NewIdentityStep(svidConn *grpc.ClientConn, trustDomain string) *IdentityStep {
	return &IdentityStep{
		svidConn:    svidConn,
		trustDomain: trustDomain,
	}
}

func (s *IdentityStep) Name() string { return "identity" }

func (s *IdentityStep) Execute(ctx context.Context, env *environment.Environment, store environment.StateWriter) error {
	if env.DryRun {
		time.Sleep(1 * time.Second)
		env.SPIREEntryIDs = []string{"entry-app-dryrun-" + env.ID[:8], "entry-db-dryrun-" + env.ID[:8]}
		env.SVIDExchangePolicyName = "policy-env-" + env.ID[:8]
		return store.Put(env)
	}

	shortID := env.ID[:8]
	appSPIFFEID := fmt.Sprintf("spiffe://%s/env-%s/app", s.trustDomain, shortID)
	dbSPIFFEID := fmt.Sprintf("spiffe://%s/env-%s/db-proxy", s.trustDomain, shortID)
	policyName := "policy-env-" + shortID

	if s.svidConn == nil {
		return fmt.Errorf("svid-exchange connection not configured (set SVIDEXCHANGE_ADDR)")
	}

	// Create svid-exchange policy allowing the app to exchange tokens for db-proxy.
	adminClient := adminv1.NewPolicyAdminClient(s.svidConn)
	_, err := adminClient.CreatePolicy(ctx, &adminv1.CreatePolicyRequest{
		Rule: &adminv1.PolicyRule{
			Name:          policyName,
			Subject:       appSPIFFEID,
			Target:        dbSPIFFEID,
			AllowedScopes: []string{"read", "write"},
			MaxTtl:        3600,
		},
	})
	if err != nil {
		return fmt.Errorf("create svid-exchange policy: %w", err)
	}
	env.SVIDExchangePolicyName = policyName

	// SPIRE workload entries are registered via the k8s workload attestor
	// (configured in k8s/spire/spire-server.yaml). We record their logical IDs
	// here so compensation can trace what was registered.
	env.SPIREEntryIDs = []string{
		fmt.Sprintf("spiffe://%s/env-%s/app", s.trustDomain, shortID),
		fmt.Sprintf("spiffe://%s/env-%s/db-proxy", s.trustDomain, shortID),
	}

	return store.Put(env)
}

func (s *IdentityStep) Compensate(ctx context.Context, env *environment.Environment, store environment.StateWriter) error {
	if env.DryRun {
		time.Sleep(1 * time.Second)
		env.SPIREEntryIDs = nil
		env.SVIDExchangePolicyName = ""
		return store.Put(env)
	}

	if env.SVIDExchangePolicyName != "" {
		if s.svidConn == nil {
			return fmt.Errorf("svid-exchange connection not configured (set SVIDEXCHANGE_ADDR)")
		}
		adminClient := adminv1.NewPolicyAdminClient(s.svidConn)
		if _, err := adminClient.DeletePolicy(ctx, &adminv1.DeletePolicyRequest{
			Name: env.SVIDExchangePolicyName,
		}); err != nil {
			return fmt.Errorf("delete svid-exchange policy: %w", err)
		}
		env.SVIDExchangePolicyName = ""
	}

	env.SPIREEntryIDs = nil
	return store.Put(env)
}

func (s *IdentityStep) IsAlreadyDone(_ context.Context, env *environment.Environment) (bool, error) {
	return env.SVIDExchangePolicyName != "", nil
}
