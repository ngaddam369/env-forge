// Package adminclient provides a thin wrapper around the svid-exchange
// PolicyAdmin gRPC service for runtime policy management and token revocation.
package adminclient

import (
	"context"
	"fmt"

	adminv1 "github.com/ngaddam369/svid-exchange/proto/admin/v1"
	"google.golang.org/grpc"
)

// Client wraps the svid-exchange admin gRPC API.
type Client struct {
	inner   adminv1.PolicyAdminClient
	conn    *grpc.ClientConn
	ownConn bool // true only when New() created the connection
}

// New dials the svid-exchange admin endpoint using the provided dial option
// (e.g. grpc.WithTransportCredentials for mTLS or insecure for local dev).
func New(addr string, opt grpc.DialOption) (*Client, error) {
	conn, err := grpc.NewClient(addr, opt)
	if err != nil {
		return nil, fmt.Errorf("dial svid-exchange admin %s: %w", addr, err)
	}
	return &Client{inner: adminv1.NewPolicyAdminClient(conn), conn: conn, ownConn: true}, nil
}

// NewFromConn creates a Client from an existing gRPC connection.
// The caller retains ownership of the connection and is responsible for closing it.
func NewFromConn(conn *grpc.ClientConn) *Client {
	return &Client{inner: adminv1.NewPolicyAdminClient(conn), conn: conn}
}

// Close releases the gRPC connection when this client owns it (created via New).
// No-op for clients created via NewFromConn — the caller closes the connection.
func (c *Client) Close() error {
	if c.ownConn {
		return c.conn.Close()
	}
	return nil
}

// GetPolicy returns the policy entry with the given name by scanning ListPolicies.
// svid-exchange does not expose a dedicated GetPolicy RPC; this wraps ListPolicies.
func (c *Client) GetPolicy(ctx context.Context, name string) (*adminv1.PolicyEntry, error) {
	resp, err := c.inner.ListPolicies(ctx, &adminv1.ListPoliciesRequest{})
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range resp.Policies {
		if p.Rule != nil && p.Rule.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("policy %q not found", name)
}

// ListPolicies returns all active policies (YAML-sourced + dynamic).
func (c *Client) ListPolicies(ctx context.Context) ([]*adminv1.PolicyEntry, error) {
	resp, err := c.inner.ListPolicies(ctx, &adminv1.ListPoliciesRequest{})
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	return resp.Policies, nil
}

// ReloadPolicy re-reads the YAML policy file and merges it with dynamic
// policies atomically. Use this after CreatePolicy or DeletePolicy to ensure
// the in-memory cache is consistent with persisted state.
func (c *Client) ReloadPolicy(ctx context.Context) error {
	_, err := c.inner.ReloadPolicy(ctx, &adminv1.ReloadPolicyRequest{})
	if err != nil {
		return fmt.Errorf("reload policies: %w", err)
	}
	return nil
}

// RevokeToken permanently revokes a token before its natural expiry.
// tokenID is the jti claim from ExchangeResponse.token_id.
// expiresAt is the natural expiry Unix timestamp from ExchangeResponse.expires_at.
func (c *Client) RevokeToken(ctx context.Context, tokenID string, expiresAt int64) error {
	_, err := c.inner.RevokeToken(ctx, &adminv1.RevokeTokenRequest{
		TokenId:   tokenID,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

// ListRevokedTokens returns all tokens that have been explicitly revoked and
// have not yet reached their natural expiry.
func (c *Client) ListRevokedTokens(ctx context.Context) ([]*adminv1.RevokedToken, error) {
	resp, err := c.inner.ListRevokedTokens(ctx, &adminv1.ListRevokedTokensRequest{})
	if err != nil {
		return nil, fmt.Errorf("list revoked tokens: %w", err)
	}
	return resp.Tokens, nil
}
