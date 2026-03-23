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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	awsclients "github.com/ngaddam369/env-forge/internal/aws"
	"github.com/ngaddam369/env-forge/internal/conductor"
	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/server"
	"github.com/ngaddam369/env-forge/internal/steps"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	root := &cobra.Command{
		Use:   "forge",
		Short: "env-forge — provision isolated AWS developer environments via saga-conductor",
	}
	root.AddCommand(
		newServeCmd(),
		newCreateCmd(),
		newDestroyCmd(),
		newListCmd(),
		newStatusCmd(),
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ── serve ────────────────────────────────────────────────────────────────────

func newServeCmd() *cobra.Command {
	var (
		addr          string
		dbPath        string
		svidAddr      string
		trustDomain   string
		conductorAddr string
		selfURL       string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the step HTTP server (saga-conductor calls this)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			store, err := environment.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer store.Close() //nolint:errcheck

			// Load AWS clients only when credentials are available.
			var awsC *awsclients.Clients
			if os.Getenv("AWS_REGION") != "" {
				awsC, err = awsclients.LoadClients(ctx)
				if err != nil {
					log.Warn().Err(err).Msg("AWS clients unavailable — steps will fail unless dry-run mode was used at create time")
				}
			}

			// gRPC connection to svid-exchange admin API.
			var svidConn *grpc.ClientConn
			if svidAddr != "" {
				svidConn, err = grpc.NewClient(svidAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					return fmt.Errorf("dial svid-exchange: %w", err)
				}
				defer svidConn.Close() //nolint:errcheck
			}

			allSteps := buildSteps(awsC, svidConn, trustDomain)

			// Conductor client enables the POST /envs/create admin endpoint.
			var provisioner server.Provisioner
			if conductorAddr != "" {
				c, err := conductor.New(conductorAddr, selfURL)
				if err != nil {
					return fmt.Errorf("connect to conductor: %w", err)
				}
				defer c.Close() //nolint:errcheck
				provisioner = &conductorProvisioner{c: c}
			}

			srv := server.New(store, allSteps, provisioner, log.Logger)
			log.Info().Str("addr", addr).Msg("step server listening")
			return srv.ListenAndServe(ctx, addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", envOrDefault("STEP_ADDR", ":9090"), "HTTP listen address")
	cmd.Flags().StringVar(&dbPath, "db", envOrDefault("DB_PATH", "env-forge.db"), "BoltDB path")
	cmd.Flags().StringVar(&svidAddr, "svid-exchange-addr", envOrDefault("SVIDEXCHANGE_ADDR", ""), "svid-exchange admin gRPC address")
	cmd.Flags().StringVar(&trustDomain, "trust-domain", envOrDefault("TRUST_DOMAIN", "cluster.local"), "SPIFFE trust domain")
	cmd.Flags().StringVar(&conductorAddr, "conductor", envOrDefault("CONDUCTOR_ADDR", "localhost:8080"), "saga-conductor gRPC address")
	cmd.Flags().StringVar(&selfURL, "self-url", envOrDefault("SELF_URL", "http://localhost:9090"), "This server's HTTP base URL (reachable by conductor)")
	return cmd
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
				return fmt.Errorf("POST /envs/create: %w (is forge serve running at %s?)", err, forgeURL)
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
	cmd.Flags().StringVar(&forgeURL, "forge-url", envOrDefault("SELF_URL", "http://localhost:9090"), "forge serve HTTP base URL")
	if err := cmd.MarkFlagRequired("owner"); err != nil {
		panic(err) // only fails if "owner" flag was not registered above
	}
	return cmd
}

// ── destroy ──────────────────────────────────────────────────────────────────

func newDestroyCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "destroy <env-id-prefix>",
		Short: "Destroy a provisioned environment (triggers saga compensation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := environment.OpenReadOnly(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer store.Close() //nolint:errcheck

			env, err := resolveEnv(store, args[0])
			if err != nil {
				return err
			}

			if env.SagaID == "" {
				return fmt.Errorf("environment %s has no associated saga", env.ID[:8])
			}

			fmt.Printf("Destroying environment %s — saga %s will be aborted.\n", env.ID[:8], env.SagaID)
			fmt.Println("(Use `forge status <env-id>` to monitor compensation progress.)")
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", envOrDefault("DB_PATH", "env-forge.db"), "BoltDB path")
	return cmd
}

// ── list ─────────────────────────────────────────────────────────────────────

func newListCmd() *cobra.Command {
	var (
		dbPath string
		status string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all environments",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := environment.OpenReadOnly(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer store.Close() //nolint:errcheck

			envs, err := store.List(status)
			if err != nil {
				return err
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
	cmd.Flags().StringVar(&dbPath, "db", envOrDefault("DB_PATH", "env-forge.db"), "BoltDB path")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (provisioning|ready|failed|destroyed)")
	return cmd
}

// ── status ────────────────────────────────────────────────────────────────────

func newStatusCmd() *cobra.Command {
	var (
		dbPath        string
		conductorAddr string
	)
	cmd := &cobra.Command{
		Use:   "status <env-id-prefix>",
		Short: "Show full status of an environment and its saga",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			store, err := environment.OpenReadOnly(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer store.Close() //nolint:errcheck

			env, err := resolveEnv(store, args[0])
			if err != nil {
				return err
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
	cmd.Flags().StringVar(&dbPath, "db", envOrDefault("DB_PATH", "env-forge.db"), "BoltDB path")
	cmd.Flags().StringVar(&conductorAddr, "conductor", envOrDefault("CONDUCTOR_ADDR", ""), "saga-conductor gRPC address (optional)")
	return cmd
}

// ── helpers ───────────────────────────────────────────────────────────────────

// conductorProvisioner adapts *conductor.Client to the server.Provisioner interface.
type conductorProvisioner struct{ c *conductor.Client }

func (p *conductorProvisioner) Provision(ctx context.Context, env *environment.Environment) error {
	_, err := p.c.Provision(ctx, env)
	return err
}

// buildSteps constructs the ordered slice of saga steps. When awsC is nil
// (no AWS credentials), steps will use dry-run mode based on env.DryRun.
func buildSteps(awsC *awsclients.Clients, svidConn *grpc.ClientConn, trustDomain string) []steps.Step {
	svidExchangeAddr := envOrDefault("SVIDEXCHANGE_ADDR", "")
	localEnvDir := envOrDefault("LOCAL_ENV_DIR", ".")
	region := envOrDefault("AWS_REGION", "us-east-1")

	if awsC != nil {
		region = awsC.Region
	}

	return []steps.Step{
		steps.NewVPCStep(awsClients(awsC).EC2),
		steps.NewRDSStep(awsClients(awsC).RDS),
		steps.NewEC2Step(awsClients(awsC).EC2),
		steps.NewS3Step(awsClients(awsC).S3, region),
		steps.NewIdentityStep(svidConn, trustDomain),
		steps.NewConfigStep(awsClients(awsC).S3, svidExchangeAddr, trustDomain, localEnvDir),
		steps.NewHealthStep(),
		steps.NewRegistryStep(),
	}
}

// awsClients returns awsC if non-nil, otherwise a zero-value Clients so callers
// can safely access nil EC2/RDS/S3 fields (steps check env.DryRun first).
func awsClients(c *awsclients.Clients) *awsclients.Clients {
	if c != nil {
		return c
	}
	return &awsclients.Clients{}
}

// resolveEnv finds an environment whose ID starts with the given prefix.
func resolveEnv(store *environment.Store, prefix string) (*environment.Environment, error) {
	envs, err := store.List("")
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	for _, e := range envs {
		if len(e.ID) >= len(prefix) && e.ID[:len(prefix)] == prefix {
			return e, nil
		}
	}
	return nil, fmt.Errorf("environment with prefix %q not found", prefix)
}

// printSagaSteps prints a step-by-step execution trace for a saga.
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
