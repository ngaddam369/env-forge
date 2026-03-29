package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaddam369/env-forge/internal/environment"
	adminv1 "github.com/ngaddam369/svid-exchange/proto/admin/v1"
	"google.golang.org/grpc"
)

// IdentityStep registers workload identities for the provisioned environment:
//  1. Calls the svid-exchange admin gRPC API to create an exchange policy
//     allowing the app identity to obtain tokens scoped to the db-proxy.
//  2. Verifies the policy was persisted by calling GetPolicy (via ListPolicies).
//  3. Calls ReloadPolicy to atomically merge the new dynamic policy with the
//     YAML-sourced policies, ensuring immediate consistency.
//
// Compensation deletes the policy and reloads to propagate the removal.
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
	// Skip real svid-exchange calls only when both dry-run AND no connection is configured
	// (local dev mode). When svidConn is set (minikube), always call the real API so the
	// dynamic policy is visible in ListPolicies — that's the whole point of Demo 3.
	if env.DryRun && s.svidConn == nil {
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

	adminClient := adminv1.NewPolicyAdminClient(s.svidConn)

	// 1. Create the svid-exchange policy.
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

	// 2. Verify the policy was persisted (GetPolicy via ListPolicies + filter).
	listResp, err := adminClient.ListPolicies(ctx, &adminv1.ListPoliciesRequest{})
	if err != nil {
		return fmt.Errorf("list policies (verify): %w", err)
	}
	const wantMaxTTL = int32(3600)
	wantScopes := []string{"read", "write"}
	var verified bool
	for _, p := range listResp.Policies {
		if p.Rule == nil || p.Rule.Name != policyName {
			continue
		}
		gotScopes := strings.Join(p.Rule.AllowedScopes, ",")
		wantScopesStr := strings.Join(wantScopes, ",")
		if gotScopes != wantScopesStr {
			return fmt.Errorf("policy %q has unexpected scopes %q (want %q)", policyName, gotScopes, wantScopesStr)
		}
		if p.Rule.MaxTtl != wantMaxTTL {
			return fmt.Errorf("policy %q has unexpected max_ttl %d (want %d)", policyName, p.Rule.MaxTtl, wantMaxTTL)
		}
		verified = true
		break
	}
	if !verified {
		return fmt.Errorf("policy %q not found after creation", policyName)
	}

	// 3. Reload policies so the new dynamic entry is active immediately.
	if _, err := adminClient.ReloadPolicy(ctx, &adminv1.ReloadPolicyRequest{}); err != nil {
		return fmt.Errorf("reload policies: %w", err)
	}

	env.SPIREEntryIDs = []string{
		fmt.Sprintf("spiffe://%s/env-%s/app", s.trustDomain, shortID),
		fmt.Sprintf("spiffe://%s/env-%s/db-proxy", s.trustDomain, shortID),
	}

	return store.Put(env)
}

func (s *IdentityStep) Compensate(ctx context.Context, env *environment.Environment, store environment.StateWriter) error {
	if env.DryRun && s.svidConn == nil {
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

		// Reload so the deletion is reflected in the active policy set immediately.
		if _, err := adminClient.ReloadPolicy(ctx, &adminv1.ReloadPolicyRequest{}); err != nil {
			return fmt.Errorf("reload policies after deletion: %w", err)
		}
	}

	env.SPIREEntryIDs = nil
	return store.Put(env)
}

func (s *IdentityStep) IsAlreadyDone(_ context.Context, env *environment.Environment) (bool, error) {
	return env.SVIDExchangePolicyName != "", nil
}
