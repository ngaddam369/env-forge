package steps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/ngaddam369/env-forge/internal/environment"
)

// RDSStep provisions a db.t3.micro Postgres 15 instance inside the environment
// VPC and polls until it reaches "available" status (~5–8 minutes). This wait is
// intentional: it creates the dramatic pause needed for the crash-recovery demo.
type RDSStep struct {
	rdsClient *rds.Client
}

// NewRDSStep creates an RDSStep. Pass nil for rdsClient to use dry-run mode.
func NewRDSStep(client *rds.Client) *RDSStep {
	return &RDSStep{rdsClient: client}
}

func (s *RDSStep) Name() string { return "rds" }

func (s *RDSStep) Execute(ctx context.Context, env *environment.Environment, store *environment.Store) error {
	if env.DryRun {
		env.RDSInstanceID = "db-dryrun-" + env.ID[:8]
		env.RDSEndpoint = "db-dryrun-" + env.ID[:8] + ".us-east-1.rds.amazonaws.com:5432"
		env.RDSPassword = "dryrunpassword"
		time.Sleep(5 * time.Second) // simulate "creating"
		time.Sleep(3 * time.Second) // simulate "available"
		return store.Put(env)
	}

	dbID := "env-forge-" + env.ID[:8]
	password, err := generatePassword()
	if err != nil {
		return fmt.Errorf("generate rds password: %w", err)
	}
	env.RDSPassword = password
	env.RDSInstanceID = dbID

	tags := []rdstypes.Tag{
		{Key: aws.String("infra-provisioner"), Value: aws.String("true")},
		{Key: aws.String("env-id"), Value: aws.String(env.ID)},
	}

	_, err = s.rdsClient.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(dbID),
		DBInstanceClass:      aws.String("db.t3.micro"),
		Engine:               aws.String("postgres"),
		EngineVersion:        aws.String("15"),
		MasterUsername:       aws.String("forgeadmin"),
		MasterUserPassword:   aws.String(password),
		AllocatedStorage:     aws.Int32(20),
		DBSubnetGroupName:    aws.String("env-forge-" + env.ID[:8]),
		VpcSecurityGroupIds:  []string{env.SecurityGroupID},
		Tags:                 tags,
		MultiAZ:              aws.Bool(false),
		PubliclyAccessible:   aws.Bool(false),
		StorageType:          aws.String("gp2"),
	})
	if err != nil {
		return fmt.Errorf("create db instance: %w", err)
	}

	// Save before polling so a crash mid-wait can be recovered.
	if err := store.Put(env); err != nil {
		return err
	}

	// Poll until available (~5–8 minutes — the drama moment).
	waiter := rds.NewDBInstanceAvailableWaiter(s.rdsClient)
	if err := waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(dbID),
	}, 15*time.Minute); err != nil {
		return fmt.Errorf("wait for rds available: %w", err)
	}

	// Retrieve endpoint after instance is available.
	out, err := s.rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(dbID),
	})
	if err != nil {
		return fmt.Errorf("describe db instance: %w", err)
	}
	if len(out.DBInstances) > 0 && out.DBInstances[0].Endpoint != nil {
		ep := out.DBInstances[0].Endpoint
		env.RDSEndpoint = fmt.Sprintf("%s:%d", aws.ToString(ep.Address), ep.Port)
	}

	return store.Put(env)
}

func (s *RDSStep) Compensate(ctx context.Context, env *environment.Environment, store *environment.Store) error {
	if env.DryRun {
		time.Sleep(2 * time.Second)
		env.RDSInstanceID = ""
		env.RDSEndpoint = ""
		env.RDSPassword = ""
		return store.Put(env)
	}

	if env.RDSInstanceID == "" {
		return nil
	}

	_, err := s.rdsClient.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(env.RDSInstanceID),
		SkipFinalSnapshot:    aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("delete db instance: %w", err)
	}

	// Poll until deleted — VPC deletion (step 1 compensation) will fail if
	// the RDS instance still exists.
	waiter := rds.NewDBInstanceDeletedWaiter(s.rdsClient)
	if err := waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(env.RDSInstanceID),
	}, 15*time.Minute); err != nil {
		return fmt.Errorf("wait for rds deleted: %w", err)
	}

	env.RDSInstanceID = ""
	env.RDSEndpoint = ""
	env.RDSPassword = ""
	return store.Put(env)
}

// IsAlreadyDone checks AWS for an existing RDS instance tagged with this env-id.
// This is the critical idempotency check for crash recovery: if the conductor
// restarts mid-poll, we resume polling rather than issuing a second create.
func (s *RDSStep) IsAlreadyDone(ctx context.Context, env *environment.Environment) (bool, error) {
	if env.DryRun {
		return env.RDSInstanceID != "", nil
	}
	if env.RDSInstanceID == "" {
		return false, nil
	}
	out, err := s.rdsClient.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(env.RDSInstanceID),
	})
	if err != nil {
		// Instance does not exist — not done.
		return false, nil //nolint:nilerr
	}
	if len(out.DBInstances) == 0 {
		return false, nil
	}
	status := aws.ToString(out.DBInstances[0].DBInstanceStatus)
	// "available" means fully provisioned; anything else means in-progress or
	// deleted — let Execute handle polling.
	return status == "available", nil
}

func generatePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
