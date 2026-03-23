package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	pb "github.com/ngaddam369/saga-conductor/proto/saga/v1"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/ngaddam369/env-forge/internal/conductor"
	"github.com/ngaddam369/env-forge/internal/environment"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	root := &cobra.Command{
		Use:   "forge",
		Short: "env-forge CLI — provision isolated AWS developer environments",
	}
	root.AddCommand(
		newCreateCmd(),
		newDestroyCmd(),
		newListCmd(),
		newStatusCmd(),
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
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
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			reqBody, err := json.Marshal(map[string]any{
				"owner":          owner,
				"dry_run":        dryRun,
				"fail_at_health": failAtHealth,
			})
			if err != nil {
				return fmt.Errorf("marshal request: %w", err)
			}

			resp, err := http.Post(forgeURL+"/envs/create", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("POST /envs/create: %w (is forge-api running at %s?)", err, forgeURL)
			}
			defer resp.Body.Close() //nolint:errcheck

			if resp.StatusCode != http.StatusAccepted {
				var msg [512]byte
				n, err := resp.Body.Read(msg[:])
				if err != nil && n == 0 {
					return fmt.Errorf("create failed (%d)", resp.StatusCode)
				}
				return fmt.Errorf("create failed (%d): %s", resp.StatusCode, string(msg[:n]))
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

				r, err := http.Get(forgeURL + "/envs/" + created.EnvID) //nolint:noctx
				if err != nil {
					return fmt.Errorf("poll status: %w", err)
				}
				var env environment.Environment
				decodeErr := json.NewDecoder(r.Body).Decode(&env)
				r.Body.Close() //nolint:errcheck
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
			r, err := http.Get(forgeURL + "/envs/" + args[0]) //nolint:noctx
			if err != nil {
				return fmt.Errorf("get environment: %w", err)
			}
			var env environment.Environment
			decodeErr := json.NewDecoder(r.Body).Decode(&env)
			r.Body.Close() //nolint:errcheck
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
			resp, err := http.Get(url) //nolint:noctx
			if err != nil {
				return fmt.Errorf("list environments: %w", err)
			}
			defer resp.Body.Close() //nolint:errcheck
			var envs []*environment.Environment
			if err := json.NewDecoder(resp.Body).Decode(&envs); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tOWNER\tSTATUS\tDRY-RUN\tCREATED") //nolint:errcheck
			for _, e := range envs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\n", //nolint:errcheck
					e.ID[:8], e.Owner, e.Status, e.DryRun,
					e.CreatedAt.Format(time.RFC3339),
				)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (provisioning|ready|failed|destroyed)")
	return cmd
}

// ── status ────────────────────────────────────────────────────────────────────

func newStatusCmd() *cobra.Command {
	var (
		forgeURL      string
		conductorAddr string
	)
	cmd := &cobra.Command{
		Use:   "status <env-id-prefix>",
		Short: "Show full status of an environment and its saga",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			r, err := http.Get(forgeURL + "/envs/" + args[0]) //nolint:noctx
			if err != nil {
				return fmt.Errorf("get environment: %w", err)
			}
			var env environment.Environment
			decodeErr := json.NewDecoder(r.Body).Decode(&env)
			r.Body.Close() //nolint:errcheck
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

			if env.SagaID != "" && conductorAddr != "" {
				c, err := conductor.New(conductorAddr, "")
				if err == nil {
					defer c.Close() //nolint:errcheck
					if exec, err := c.GetSaga(ctx, env.SagaID); err == nil {
						fmt.Printf("\nSaga %s — %s\n", exec.Id[:8], exec.Status)
						printSagaSteps(exec)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("FORGE_URL", "http://localhost:9090"), "forge-api HTTP base URL")
	cmd.Flags().StringVar(&conductorAddr, "conductor", envOrDefault("CONDUCTOR_ADDR", ""), "saga-conductor gRPC address (optional)")
	return cmd
}

// ── helpers ───────────────────────────────────────────────────────────────────

func printSagaSteps(exec *pb.SagaExecution) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  STEP\tSTATUS\tSTARTED\tCOMPLETED\tERROR") //nolint:errcheck
	for _, st := range exec.Steps {
		started := ""
		if st.StartedAt != nil {
			started = time.Unix(st.StartedAt.Seconds, 0).Format("15:04:05")
		}
		completed := ""
		if st.CompletedAt != nil {
			completed = time.Unix(st.CompletedAt.Seconds, 0).Format("15:04:05")
		}
		errMsg := ""
		if st.Error != "" {
			errMsg = st.Error
			if len(errMsg) > 60 {
				errMsg = errMsg[:57] + "..."
			}
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", st.Name, st.Status, started, completed, errMsg) //nolint:errcheck
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "flush tabwriter: %v\n", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
