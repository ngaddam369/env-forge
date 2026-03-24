package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	pb "github.com/ngaddam369/saga-conductor/proto/saga/v1"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/ngaddam369/env-forge/internal/environment"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	root := &cobra.Command{
		Use:   "forge",
		Short: "env-forge CLI — provision isolated AWS developer environments",
	}

	// Environment management
	root.AddCommand(
		newCreateCmd(),
		newDestroyCmd(),
		newListCmd(),
		newStatusCmd(),
	)

	// Saga management (saga-conductor features)
	sagasCmd := &cobra.Command{
		Use:   "sagas",
		Short: "List and manage saga executions (saga-conductor)",
	}
	sagasCmd.AddCommand(newSagasListCmd(), newSagaAbortCmd())
	root.AddCommand(sagasCmd)

	// svid-exchange policy management
	policiesCmd := &cobra.Command{
		Use:   "policies",
		Short: "Manage svid-exchange exchange policies",
	}
	policiesCmd.AddCommand(newPoliciesListCmd(), newPoliciesReloadCmd())
	root.AddCommand(policiesCmd)

	// svid-exchange token management
	tokensCmd := &cobra.Command{
		Use:   "tokens",
		Short: "Manage svid-exchange tokens",
	}
	tokensCmd.AddCommand(newTokenRevokeCmd(), newTokensRevokedCmd())
	root.AddCommand(tokensCmd)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

// doGet makes a GET request with context and returns the response.
// Caller is responsible for closing resp.Body.
func doGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET request: %w", err)
	}
	return http.DefaultClient.Do(req)
}

// doPost makes a POST request with context and an optional JSON body.
// Caller is responsible for closing resp.Body.
func doPost(ctx context.Context, url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("build POST request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return http.DefaultClient.Do(req)
}

// closeBody closes an HTTP response body and logs any error to stderr.
// Call as defer closeBody(resp.Body) immediately after a successful Do.
func closeBody(body io.ReadCloser) {
	if err := body.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close response body: %v\n", err)
	}
}

// ── tabwriter helper ──────────────────────────────────────────────────────────

// table wraps tabwriter.Writer and accumulates write errors so callers only
// need to check one error at the end via flush().
type table struct {
	w   *tabwriter.Writer
	err error
}

func newTable() *table {
	return &table{w: tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)}
}

func (t *table) row(format string, args ...any) {
	if t.err != nil {
		return
	}
	_, t.err = fmt.Fprintf(t.w, format, args...)
}

func (t *table) flush() error {
	if t.err != nil {
		return t.err
	}
	return t.w.Flush()
}

// ── create ───────────────────────────────────────────────────────────────────

func newCreateCmd() *cobra.Command {
	var (
		owner        string
		dryRun       bool
		failAtHealth bool
		forgeURL     string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a new developer environment",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			reqBody, err := json.Marshal(map[string]any{
				"owner":          owner,
				"dry_run":        dryRun,
				"fail_at_health": failAtHealth,
			})
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := doPost(ctx, forgeURL+"/envs/create", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("POST /envs/create: %w (is forge-api running at %s?)", err, forgeURL)
			}
			defer closeBody(resp.Body)

			if resp.StatusCode != http.StatusAccepted {
				body, readErr := io.ReadAll(resp.Body)
				if readErr != nil {
					return fmt.Errorf("create failed (%d): (could not read body: %v)", resp.StatusCode, readErr)
				}
				return fmt.Errorf("create failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}

			var created struct {
				EnvID string `json:"env_id"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			fmt.Printf("Provisioning environment %s (owner: %s, dry-run: %v)\n", created.EnvID[:8], owner, dryRun)
			fmt.Printf("Watching for completion...\n\n")

			// Poll /envs/{id} until the saga reaches a terminal state.
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(2 * time.Second):
				}

				r, err := doGet(ctx, forgeURL+"/envs/"+created.EnvID)
				if err != nil {
					return fmt.Errorf("poll status: %w", err)
				}
				var env environment.Environment
				decodeErr := json.NewDecoder(r.Body).Decode(&env)
				closeBody(r.Body)
				if decodeErr != nil {
					return fmt.Errorf("decode env: %w", decodeErr)
				}

				if env.Status != environment.StatusProvisioning {
					fmt.Printf("\nEnvironment %s — status: %s\n", env.ID[:8], env.Status)
					return nil
				}
				fmt.Printf("  [%s] still provisioning...\r", time.Now().Format("15:04:05"))
			}
		},
	}
	cmd.Flags().StringVar(&owner, "owner", "", "Owner name (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Simulate AWS calls with delays instead of real API calls")
	cmd.Flags().BoolVar(&failAtHealth, "fail-at-health", false, "Inject failure at health validation step (demo moment 2)")
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	if err := cmd.MarkFlagRequired("owner"); err != nil {
		panic(err)
	}
	return cmd
}

// ── destroy ──────────────────────────────────────────────────────────────────

func newDestroyCmd() *cobra.Command {
	var forgeURL string
	cmd := &cobra.Command{
		Use:   "destroy <env-id-prefix>",
		Short: "Destroy a provisioned environment (triggers saga compensation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := doGet(cmd.Context(), forgeURL+"/envs/"+args[0])
			if err != nil {
				return fmt.Errorf("get environment: %w", err)
			}
			var env environment.Environment
			decodeErr := json.NewDecoder(r.Body).Decode(&env)
			closeBody(r.Body)
			if decodeErr != nil {
				return fmt.Errorf("decode env: %w", decodeErr)
			}
			if r.StatusCode == http.StatusNotFound {
				return fmt.Errorf("environment %q not found", args[0])
			}
			if env.SagaID == "" {
				return fmt.Errorf("environment %s has no associated saga", env.ID[:8])
			}
			fmt.Printf("Destroying environment %s — saga %s will be aborted.\n", env.ID[:8], env.SagaID)
			fmt.Println("(Use `forge status <env-id>` to monitor compensation progress.)")
			return nil
		},
	}
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	return cmd
}

// ── list ─────────────────────────────────────────────────────────────────────

func newListCmd() *cobra.Command {
	var (
		forgeURL string
		status   string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all environments",
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := forgeURL + "/envs"
			if status != "" {
				url += "?status=" + status
			}
			resp, err := doGet(cmd.Context(), url)
			if err != nil {
				return fmt.Errorf("list environments: %w", err)
			}
			defer closeBody(resp.Body)
			var envs []*environment.Environment
			if err := json.NewDecoder(resp.Body).Decode(&envs); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			t := newTable()
			t.row("ID\tOWNER\tSTATUS\tDRY-RUN\tCREATED\n")
			for _, e := range envs {
				t.row("%s\t%s\t%s\t%v\t%s\n",
					e.ID[:8], e.Owner, e.Status, e.DryRun,
					e.CreatedAt.Format(time.RFC3339),
				)
			}
			return t.flush()
		},
	}
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (provisioning|ready|failed|destroyed)")
	return cmd
}

// ── status ────────────────────────────────────────────────────────────────────

func newStatusCmd() *cobra.Command {
	var forgeURL string
	cmd := &cobra.Command{
		Use:   "status <env-id-prefix>",
		Short: "Show full status of an environment and its saga steps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := doGet(cmd.Context(), forgeURL+"/envs/"+args[0])
			if err != nil {
				return fmt.Errorf("get environment: %w", err)
			}
			var env environment.Environment
			decodeErr := json.NewDecoder(r.Body).Decode(&env)
			closeBody(r.Body)
			if decodeErr != nil {
				return fmt.Errorf("decode env: %w", decodeErr)
			}

			fmt.Printf("Environment: %s\n", env.ID)
			fmt.Printf("  Owner:      %s\n", env.Owner)
			fmt.Printf("  Status:     %s\n", env.Status)
			fmt.Printf("  DryRun:     %v\n", env.DryRun)
			fmt.Printf("  Created:    %s\n", env.CreatedAt.Format(time.RFC3339))
			fmt.Printf("  SagaID:     %s\n", env.SagaID)
			fmt.Printf("  VPC:        %s\n", env.VPCID)
			fmt.Printf("  RDS:        %s\n", env.RDSEndpoint)
			fmt.Printf("  EC2:        %s (%s)\n", env.EC2InstanceID, env.EC2PublicIP)
			fmt.Printf("  S3 Bucket:  %s\n", env.S3BucketName)
			fmt.Printf("  Policy:     %s\n", env.SVIDExchangePolicyName)

			// Fetch saga step detail from forge-api proxy endpoint.
			// Demonstrates: GetSaga, StepExecution.error_detail, failed_step.
			if env.SagaID != "" {
				sr, err := doGet(cmd.Context(), forgeURL+"/envs/"+args[0]+"/saga")
				if err == nil && sr.StatusCode == http.StatusOK {
					var exec pb.SagaExecution
					if decErr := json.NewDecoder(sr.Body).Decode(&exec); decErr == nil {
						fmt.Printf("\nSaga %s — %s", exec.Id[:8], exec.Status)
						if exec.FailedStep != "" {
							fmt.Printf("  (failed at: %s)", exec.FailedStep)
						}
						fmt.Println()
						if printErr := printSagaSteps(&exec); printErr != nil {
							return printErr
						}
					}
					closeBody(sr.Body)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	return cmd
}

// ── sagas list ────────────────────────────────────────────────────────────────

func newSagasListCmd() *cobra.Command {
	var (
		forgeURL string
		status   string
		pageSize int
		cursor   string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List saga executions with optional status filter and pagination",
		Long: `List sagas via saga-conductor ListSagas RPC (proxied through forge-api).

Demonstrates: ListSagas with status filtering, cursor-based pagination,
and SagaExecution.failed_step field.

Status values: RUNNING, COMPLETED, FAILED, ABORTED, COMPENSATING, COMPENSATION_FAILED`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url := forgeURL + "/sagas"
			sep := "?"
			if status != "" {
				url += sep + "status=" + status
				sep = "&"
			}
			if pageSize > 0 {
				url += fmt.Sprintf("%spage_size=%d", sep, pageSize)
				sep = "&"
			}
			if cursor != "" {
				url += sep + "cursor=" + cursor
			}

			resp, err := doGet(cmd.Context(), url)
			if err != nil {
				return fmt.Errorf("list sagas: %w", err)
			}
			defer closeBody(resp.Body)

			if resp.StatusCode == http.StatusServiceUnavailable {
				return fmt.Errorf("forge-api has no conductor connection (start with CONDUCTOR_ADDR set)")
			}

			var result struct {
				Sagas         []*pb.SagaExecution `json:"sagas"`
				NextPageToken string              `json:"next_page_token"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			t := newTable()
			t.row("SAGA ID\tSTATUS\tFAILED STEP\tCREATED\n")
			for _, s := range result.Sagas {
				id := s.Id
				if len(id) > 8 {
					id = id[:8]
				}
				created := ""
				if s.CreatedAt != nil {
					created = time.Unix(s.CreatedAt.Seconds, 0).Format(time.RFC3339)
				}
				t.row("%s\t%s\t%s\t%s\n", id, s.Status, s.FailedStep, created)
			}
			if err := t.flush(); err != nil {
				return err
			}

			if result.NextPageToken != "" {
				fmt.Printf("\nNext page: --cursor=%s\n", result.NextPageToken)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	cmd.Flags().StringVar(&status, "status", "", "Filter by saga status (RUNNING, COMPLETED, FAILED, ABORTED, ...)")
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "Page size (default 100)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Pagination cursor from previous response")
	return cmd
}

// ── sagas abort ───────────────────────────────────────────────────────────────

func newSagaAbortCmd() *cobra.Command {
	var forgeURL string
	cmd := &cobra.Command{
		Use:   "abort <env-id-prefix>",
		Short: "Forcibly abort the saga for an environment (no compensation triggered)",
		Long: `Abort a saga via saga-conductor AbortSaga RPC (proxied through forge-api).

Demonstrates: AbortSaga — moves saga to ABORTED state immediately without
triggering any compensation steps. Unlike a failure, ABORTED is a deliberate
operator action. Useful for environments stuck in COMPENSATING or RUNNING.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := forgeURL + "/envs/" + args[0] + "/abort"
			resp, err := doPost(cmd.Context(), url, "", nil)
			if err != nil {
				return fmt.Errorf("abort saga: %w", err)
			}
			defer closeBody(resp.Body)

			if resp.StatusCode == http.StatusServiceUnavailable {
				return fmt.Errorf("forge-api has no conductor connection")
			}
			if resp.StatusCode != http.StatusOK {
				body, readErr := io.ReadAll(resp.Body)
				if readErr != nil {
					return fmt.Errorf("abort failed (%d): (could not read body: %v)", resp.StatusCode, readErr)
				}
				return fmt.Errorf("abort failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
			}

			var exec pb.SagaExecution
			if err := json.NewDecoder(resp.Body).Decode(&exec); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			fmt.Printf("Saga %s aborted — status: %s\n", exec.Id[:8], exec.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	return cmd
}

// ── policies list ─────────────────────────────────────────────────────────────

// policyEntry mirrors the JSON returned by forge-worker GET /admin/policies.
type policyEntry struct {
	Rule   *policyRule `json:"rule"`
	Source string      `json:"source"`
}

type policyRule struct {
	Name          string   `json:"name"`
	Subject       string   `json:"subject"`
	Target        string   `json:"target"`
	AllowedScopes []string `json:"allowed_scopes"`
	MaxTtl        int32    `json:"max_ttl"`
}

func newPoliciesListCmd() *cobra.Command {
	var workerURL string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all svid-exchange exchange policies (YAML + dynamic)",
		Long: `List policies via svid-exchange PolicyAdmin.ListPolicies RPC.

Demonstrates: ListPolicies, PolicyEntry.source field ("yaml" or "dynamic"),
and the full policy rule (subject, target, scopes, max_ttl).

Each env provisioning (identity step) adds a dynamic policy. This command
shows both the static YAML-sourced policies and all dynamically created ones.

Requires: kubectl port-forward svc/forge-worker 9091:9091`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := doGet(cmd.Context(), workerURL+"/admin/policies")
			if err != nil {
				return fmt.Errorf("list policies: %w", err)
			}
			defer closeBody(resp.Body)
			if resp.StatusCode == http.StatusServiceUnavailable {
				return fmt.Errorf("forge-worker has no admin connection (set SVIDEXCHANGE_ADMIN_ADDR)")
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("list policies: status %d", resp.StatusCode)
			}

			var result struct {
				Policies []policyEntry `json:"policies"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return fmt.Errorf("decode policies: %w", err)
			}

			t := newTable()
			t.row("NAME\tSOURCE\tSUBJECT\tTARGET\tSCOPES\tMAX TTL\n")
			for _, p := range result.Policies {
				if p.Rule == nil {
					continue
				}
				scopes := strings.Join(p.Rule.AllowedScopes, ",")
				subj := shortenSPIFFE(p.Rule.Subject)
				tgt := shortenSPIFFE(p.Rule.Target)
				t.row("%s\t%s\t%s\t%s\t%s\t%ds\n",
					p.Rule.Name, p.Source, subj, tgt, scopes, p.Rule.MaxTtl)
			}
			return t.flush()
		},
	}
	cmd.Flags().StringVar(&workerURL, "worker-url", envOrDefault("FORGE_WORKER_URL", "http://localhost:9091"), "forge-worker HTTP base URL")
	return cmd
}

// ── policies reload ───────────────────────────────────────────────────────────

func newPoliciesReloadCmd() *cobra.Command {
	var workerURL string
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Reload svid-exchange YAML policy file without restarting the server",
		Long: `Reload policies via svid-exchange PolicyAdmin.ReloadPolicy RPC.

Demonstrates: ReloadPolicy — atomically merges the on-disk YAML policy file
with all dynamic (API-created) policies. Use this after editing the YAML file
in the ConfigMap (and rolling the pod) or to verify the reload mechanism.

Requires: kubectl port-forward svc/forge-worker 9091:9091`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := doPost(cmd.Context(), workerURL+"/admin/policies/reload", "", nil)
			if err != nil {
				return fmt.Errorf("reload policies: %w", err)
			}
			defer closeBody(resp.Body)
			if resp.StatusCode == http.StatusServiceUnavailable {
				return fmt.Errorf("forge-worker has no admin connection (set SVIDEXCHANGE_ADMIN_ADDR)")
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("reload policies: status %d", resp.StatusCode)
			}
			fmt.Println("Policies reloaded successfully.")
			return nil
		},
	}
	cmd.Flags().StringVar(&workerURL, "worker-url", envOrDefault("FORGE_WORKER_URL", "http://localhost:9091"), "forge-worker HTTP base URL")
	return cmd
}

// ── tokens revoke ─────────────────────────────────────────────────────────────

func newTokenRevokeCmd() *cobra.Command {
	var (
		workerURL string
		expiresAt int64
	)
	cmd := &cobra.Command{
		Use:   "revoke <token-jti>",
		Short: "Revoke a specific JWT by its token ID (jti claim)",
		Long: `Revoke a token via svid-exchange PolicyAdmin.RevokeToken RPC.

Demonstrates: RevokeToken — permanently denies a token before its natural
expiry. The revocation persists in BoltDB across server restarts.

The token_id (jti) is logged by svid-exchange after each exchange:
  kubectl logs -l app=svid-exchange | grep token_id

Use --expires-at to set the natural expiry Unix timestamp (from ExchangeResponse.expires_at)
so that stale revocation entries can be purged automatically.

Requires: kubectl port-forward svc/forge-worker 9091:9091`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, marshalErr := json.Marshal(map[string]any{
				"token_id":   args[0],
				"expires_at": expiresAt,
			})
			if marshalErr != nil {
				return fmt.Errorf("marshal request: %w", marshalErr)
			}
			resp, err := doPost(cmd.Context(), workerURL+"/admin/tokens/revoke",
				"application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("revoke token: %w", err)
			}
			defer closeBody(resp.Body)
			if resp.StatusCode == http.StatusServiceUnavailable {
				return fmt.Errorf("forge-worker has no admin connection (set SVIDEXCHANGE_ADMIN_ADDR)")
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("revoke token: status %d", resp.StatusCode)
			}
			fmt.Printf("Token %s revoked successfully.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&workerURL, "worker-url", envOrDefault("FORGE_WORKER_URL", "http://localhost:9091"), "forge-worker HTTP base URL")
	cmd.Flags().Int64Var(&expiresAt, "expires-at", 0, "Token natural expiry as Unix timestamp (for automatic purge)")
	return cmd
}

// ── tokens revoked ────────────────────────────────────────────────────────────

// revokedToken mirrors the JSON returned by forge-worker GET /admin/tokens/revoked.
type revokedToken struct {
	TokenId   string `json:"token_id"`
	ExpiresAt int64  `json:"expires_at"`
}

func newTokensRevokedCmd() *cobra.Command {
	var workerURL string
	cmd := &cobra.Command{
		Use:   "revoked",
		Short: "List all explicitly revoked tokens that have not yet expired",
		Long: `List revoked tokens via svid-exchange PolicyAdmin.ListRevokedTokens RPC.

Demonstrates: ListRevokedTokens — shows all tokens in the revocation list
(BoltDB-persisted). Tokens past their natural expiry are purged automatically.

Requires: kubectl port-forward svc/forge-worker 9091:9091`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := doGet(cmd.Context(), workerURL+"/admin/tokens/revoked")
			if err != nil {
				return fmt.Errorf("list revoked tokens: %w", err)
			}
			defer closeBody(resp.Body)
			if resp.StatusCode == http.StatusServiceUnavailable {
				return fmt.Errorf("forge-worker has no admin connection (set SVIDEXCHANGE_ADMIN_ADDR)")
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("list revoked tokens: status %d", resp.StatusCode)
			}

			var result struct {
				Tokens []revokedToken `json:"tokens"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				return fmt.Errorf("decode revoked tokens: %w", err)
			}

			if len(result.Tokens) == 0 {
				fmt.Println("No revoked tokens.")
				return nil
			}
			t := newTable()
			t.row("TOKEN ID\tEXPIRES AT\n")
			for _, tok := range result.Tokens {
				expires := time.Unix(tok.ExpiresAt, 0).Format(time.RFC3339)
				t.row("%s\t%s\n", tok.TokenId, expires)
			}
			return t.flush()
		},
	}
	cmd.Flags().StringVar(&workerURL, "worker-url", envOrDefault("FORGE_WORKER_URL", "http://localhost:9091"), "forge-worker HTTP base URL")
	return cmd
}

// ── helpers ───────────────────────────────────────────────────────────────────

func printSagaSteps(exec *pb.SagaExecution) error {
	t := newTable()
	t.row("  STEP\tSTATUS\tSTARTED\tCOMPLETED\tERROR\n")
	for _, st := range exec.Steps {
		started := ""
		if st.StartedAt != nil {
			started = time.Unix(st.StartedAt.Seconds, 0).Format("15:04:05")
		}
		completed := ""
		if st.CompletedAt != nil {
			completed = time.Unix(st.CompletedAt.Seconds, 0).Format("15:04:05")
		}
		errMsg := st.Error
		if len(errMsg) > 60 {
			errMsg = errMsg[:57] + "..."
		}
		// Parse error_detail JSON for structured HTTP error info.
		if len(st.ErrorDetail) > 0 {
			var detail struct {
				HTTPStatus   int    `json:"http_status_code"`
				ResponseBody string `json:"response_body"`
			}
			if json.Unmarshal(st.ErrorDetail, &detail) == nil && detail.HTTPStatus != 0 {
				body := detail.ResponseBody
				if len(body) > 40 {
					body = body[:37] + "..."
				}
				errMsg = fmt.Sprintf("HTTP %d: %s", detail.HTTPStatus, body)
			}
		}
		t.row("  %s\t%s\t%s\t%s\t%s\n", st.Name, st.Status, started, completed, errMsg)
	}
	return t.flush()
}

// shortenSPIFFE abbreviates long SPIFFE IDs for table display.
func shortenSPIFFE(id string) string {
	const prefix = "spiffe://"
	if len(id) > len(prefix) {
		id = id[len(prefix):]
	}
	if len(id) > 40 {
		id = "..." + id[len(id)-37:]
	}
	return id
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
