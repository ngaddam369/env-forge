package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Clients bundles the AWS SDK clients used across all provisioning steps.
// All clients are constructed from the same underlying config (region, creds).
type Clients struct {
	EC2    *ec2.Client
	RDS    *rds.Client
	S3     *s3.Client
	Region string
}

// LoadClients loads AWS configuration from environment variables
// (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION) and constructs
// SDK clients for EC2, RDS, and S3.
//
// In dry-run mode this function is not called — callers should pass nil Clients.
func LoadClients(ctx context.Context) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("AWS_REGION must be set")
	}
	return &Clients{
		EC2:    ec2.NewFromConfig(cfg),
		RDS:    rds.NewFromConfig(cfg),
		S3:     s3.NewFromConfig(cfg),
		Region: cfg.Region,
	}, nil
}
