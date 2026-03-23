package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ngaddam369/env-forge/internal/environment"
)

// envConfig is the JSON structure uploaded to S3 and written to the local .env.
type envConfig struct {
	EnvID            string `json:"env_id"`
	RDSEndpoint      string `json:"rds_endpoint"`
	RDSPassword      string `json:"rds_password"`
	S3BucketName     string `json:"s3_bucket_name"`
	SPIFFESocketPath string `json:"spiffe_socket_path"`
	SVIDExchangeAddr string `json:"svid_exchange_addr"`
	JWKSEndpoint     string `json:"jwks_endpoint"`
	TrustDomain      string `json:"trust_domain"`
}

// ConfigStep generates config.json from the accumulated Environment state,
// uploads it to the S3 bucket (Step 4), and writes a local .env file.
type ConfigStep struct {
	s3Client         *s3.Client
	svidExchangeAddr string
	trustDomain      string
	localEnvDir      string
}

// NewConfigStep creates a ConfigStep. Pass nil for s3Client to use dry-run mode.
func NewConfigStep(client *s3.Client, svidExchangeAddr, trustDomain, localEnvDir string) *ConfigStep {
	return &ConfigStep{
		s3Client:         client,
		svidExchangeAddr: svidExchangeAddr,
		trustDomain:      trustDomain,
		localEnvDir:      localEnvDir,
	}
}

func (s *ConfigStep) Name() string { return "config" }

func (s *ConfigStep) Execute(ctx context.Context, env *environment.Environment, store environment.StateWriter) error {
	cfg := envConfig{
		EnvID:            env.ID,
		RDSEndpoint:      env.RDSEndpoint,
		RDSPassword:      env.RDSPassword,
		S3BucketName:     env.S3BucketName,
		SPIFFESocketPath: "/run/spire/sockets/agent.sock",
		SVIDExchangeAddr: s.svidExchangeAddr,
		JWKSEndpoint:     fmt.Sprintf("http://%s/jwks", s.svidExchangeAddr),
		TrustDomain:      s.trustDomain,
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if !env.DryRun {
		// Upload config.json to S3.
		if _, err := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(env.S3BucketName),
			Key:         aws.String("config.json"),
			Body:        bytes.NewReader(data),
			ContentType: aws.String("application/json"),
		}); err != nil {
			return fmt.Errorf("upload config.json to s3: %w", err)
		}
	} else {
		time.Sleep(500 * time.Millisecond)
	}

	// Write local .env file.
	if err := os.MkdirAll(s.localEnvDir, 0o755); err != nil {
		return fmt.Errorf("create local env dir: %w", err)
	}
	envPath := filepath.Join(s.localEnvDir, "env-"+env.ID[:8]+".env")
	envContent := fmt.Sprintf(
		"ENV_ID=%s\nRDS_ENDPOINT=%s\nRDS_PASSWORD=%s\nS3_BUCKET=%s\nSPIFFE_SOCKET=%s\nSVID_EXCHANGE_ADDR=%s\nJWKS_URL=%s\n",
		cfg.EnvID, cfg.RDSEndpoint, cfg.RDSPassword, cfg.S3BucketName,
		cfg.SPIFFESocketPath, cfg.SVIDExchangeAddr, cfg.JWKSEndpoint,
	)
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		return fmt.Errorf("write .env file: %w", err)
	}
	env.LocalEnvPath = envPath

	return store.Put(env)
}

func (s *ConfigStep) Compensate(ctx context.Context, env *environment.Environment, store environment.StateWriter) error {
	if !env.DryRun && env.S3BucketName != "" {
		if _, err := s.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(env.S3BucketName),
			Key:    aws.String("config.json"),
		}); err != nil {
			return fmt.Errorf("delete config.json from s3: %w", err)
		}
	}

	if env.LocalEnvPath != "" {
		if err := os.Remove(env.LocalEnvPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove .env file: %w", err)
		}
		env.LocalEnvPath = ""
	}

	return store.Put(env)
}

func (s *ConfigStep) IsAlreadyDone(_ context.Context, env *environment.Environment) (bool, error) {
	return env.LocalEnvPath != "", nil
}
