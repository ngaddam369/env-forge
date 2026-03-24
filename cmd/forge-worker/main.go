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
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	grpccredentials "github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	svidclient "github.com/ngaddam369/svid-exchange/pkg/client"

	"github.com/ngaddam369/env-forge/internal/adminclient"
	awsclients "github.com/ngaddam369/env-forge/internal/aws"
	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/server"
	"github.com/ngaddam369/env-forge/internal/steps"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		log.Fatal().Err(err).Msg("forge-worker exited")
	}
}

func run(ctx context.Context) error {
	addr := envOrDefault("WORKER_ADDR", ":9091")
	forgeAPIURL := envOrDefault("FORGE_API_URL", "http://forge-api.default.svc.cluster.local:9090")
	spiffeSocket := envOrDefault("SPIFFE_ENDPOINT_SOCKET", "")
	svidExchangeAddr := envOrDefault("SVIDEXCHANGE_ADDR", "")
	svidAdminAddr := envOrDefault("SVIDEXCHANGE_ADMIN_ADDR", "")
	trustDomain := envOrDefault("TRUST_DOMAIN", "cluster.local")
	localEnvDir := envOrDefault("LOCAL_ENV_DIR", "/tmp/envfiles")

	// Build remote state client (calls forge-api for all env reads/writes).
	sc := buildStateClient(ctx, forgeAPIURL, spiffeSocket, svidExchangeAddr)

	// Load AWS clients if credentials available.
	var awsC *awsclients.Clients
	if os.Getenv("AWS_REGION") != "" {
		var err error
		awsC, err = awsclients.LoadClients(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("AWS clients unavailable — steps will fail unless dry-run mode was used at create time")
		}
	}

	// gRPC connection to svid-exchange admin API (for identity step + admin proxy).
	// Uses SPIFFE mTLS when the SPIRE socket is available; falls back to insecure
	// for local dev (--dry-run without SPIRE).
	var (
		svidConn *grpc.ClientConn
		ac       *adminclient.Client
	)
	if svidAdminAddr != "" {
		var adminCreds grpc.DialOption
		if spiffeSocket != "" {
			src, srcErr := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(
				workloadapi.WithAddr(spiffeSocket),
			))
			if srcErr != nil {
				log.Warn().Err(srcErr).Msg("X509Source unavailable — admin API using insecure credentials")
				adminCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
			} else {
				defer func() {
					if cerr := src.Close(); cerr != nil {
						log.Error().Err(cerr).Msg("close admin X509Source")
					}
				}()
				adminCreds = grpc.WithTransportCredentials(
					grpccredentials.MTLSClientCredentials(src, src, tlsconfig.AuthorizeAny()),
				)
			}
		} else {
			adminCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
		}
		var err error
		svidConn, err = grpc.NewClient(svidAdminAddr, adminCreds)
		if err != nil {
			return fmt.Errorf("dial svid-exchange admin: %w", err)
		}
		defer func() {
			if cerr := svidConn.Close(); cerr != nil {
				log.Error().Err(cerr).Msg("close svid-exchange admin connection")
			}
		}()
		ac = adminclient.NewFromConn(svidConn)
		log.Info().Str("admin_addr", svidAdminAddr).Msg("svid-exchange admin client ready")
	}

	allSteps := buildSteps(awsC, svidConn, trustDomain, localEnvDir)
	srv := server.New(sc, allSteps, ac, log.Logger)
	log.Info().Str("addr", addr).Msg("forge-worker listening")
	return srv.ListenAndServe(ctx, addr)
}

// buildStateClient constructs the RemoteStateClient. When SPIFFE is available,
// it wires up JWT acquisition via svid-exchange; otherwise dev mode (no auth).
func buildStateClient(ctx context.Context, apiURL, spiffeSocket, svidAddr string) *remoteStateClient {
	sc := &remoteStateClient{apiURL: apiURL}

	if spiffeSocket == "" || svidAddr == "" {
		log.Warn().Msg("SPIFFE_ENDPOINT_SOCKET or SVIDEXCHANGE_ADDR not set — calling forge-api without JWT (dev mode)")
		return sc
	}

	exchangeClient, err := svidclient.New(ctx, svidclient.Options{
		Addr:          svidAddr,
		SpiffeSocket:  spiffeSocket,
		TargetService: "spiffe://cluster.local/ns/default/sa/forge-api",
		Scopes:        []string{"env:read", "env:write"},
		TTLSeconds:    3600,
	})
	if err != nil {
		log.Warn().Err(err).Msg("svid-exchange client init failed — falling back to dev mode (no JWT)")
		return sc
	}

	sc.getToken = func(ctx context.Context) (string, error) {
		return exchangeClient.Token(ctx)
	}
	sc.closer = exchangeClient
	log.Info().Str("svid_exchange_addr", svidAddr).Msg("JWT token exchange enabled")
	return sc
}

// remoteStateClient satisfies environment.StateClient by calling forge-api's
// internal HTTP endpoints, optionally attaching a Bearer JWT.
type remoteStateClient struct {
	apiURL   string
	getToken func(ctx context.Context) (string, error) // nil → dev mode
	closer   io.Closer                                 // nil unless exchangeClient
}

func (c *remoteStateClient) Get(id string) (*environment.Environment, error) {
	req, err := http.NewRequest(http.MethodGet, c.apiURL+"/internal/envs/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET request: %w", err)
	}
	if err := c.addAuth(req); err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /internal/envs/%s: %w", id, err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Error().Err(cerr).Str("id", id).Msg("close GET response body")
		}
	}()
	if resp.StatusCode == http.StatusNotFound {
		return nil, environment.ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /internal/envs/%s: status %d", id, resp.StatusCode)
	}
	var env environment.Environment
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode env: %w", err)
	}
	return &env, nil
}

func (c *remoteStateClient) Put(env *environment.Environment) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal env: %w", err)
	}
	req, err := http.NewRequest(http.MethodPut, c.apiURL+"/internal/envs/"+env.ID, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build PUT request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.addAuth(req); err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT /internal/envs/%s: %w", env.ID, err)
	}
	if cerr := resp.Body.Close(); cerr != nil {
		log.Error().Err(cerr).Str("id", env.ID).Msg("close PUT response body")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT /internal/envs/%s: status %d", env.ID, resp.StatusCode)
	}
	return nil
}

func (c *remoteStateClient) addAuth(req *http.Request) error {
	if c.getToken == nil {
		return nil
	}
	token, err := c.getToken(req.Context())
	if err != nil {
		return fmt.Errorf("get JWT: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// buildSteps constructs the ordered slice of saga steps.
func buildSteps(awsC *awsclients.Clients, svidConn *grpc.ClientConn, trustDomain, localEnvDir string) []steps.Step {
	region := envOrDefault("AWS_REGION", "us-east-1")
	svidExchangeAddr := envOrDefault("SVIDEXCHANGE_ADDR", "")
	if awsC != nil {
		region = awsC.Region
	}
	c := awsClients(awsC)
	return []steps.Step{
		steps.NewVPCStep(c.EC2),
		steps.NewRDSStep(c.RDS),
		steps.NewEC2Step(c.EC2),
		steps.NewS3Step(c.S3, region),
		steps.NewIdentityStep(svidConn, trustDomain),
		steps.NewConfigStep(c.S3, svidExchangeAddr, trustDomain, localEnvDir),
		steps.NewHealthStep(),
		steps.NewRegistryStep(),
	}
}

// awsClients returns a safe zero-value Clients when awsC is nil.
func awsClients(c *awsclients.Clients) *awsclients.Clients {
	if c != nil {
		return c
	}
	return &awsclients.Clients{}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
