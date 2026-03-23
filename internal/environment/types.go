package environment

import "time"

// Status values for an Environment.
const (
	StatusProvisioning = "provisioning"
	StatusReady        = "ready"
	StatusFailed       = "failed"
	StatusDestroyed    = "destroyed"
)

// Environment holds all state accumulated across the 8 provisioning saga steps.
// It is persisted in BoltDB after every step so the provisioner can resume after
// a crash without creating duplicate AWS resources.
type Environment struct {
	// Identity
	ID    string `json:"id"`
	Owner string `json:"owner"`

	// Lifecycle
	Status    string    `json:"status"`
	SagaID    string    `json:"saga_id"`
	DryRun    bool      `json:"dry_run"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Step 1 — VPC + Networking
	VPCID           string `json:"vpc_id"`
	PublicSubnetID  string `json:"public_subnet_id"`
	PrivateSubnetID string `json:"private_subnet_id"`
	SecurityGroupID string `json:"security_group_id"`

	// Step 2 — RDS
	RDSInstanceID string `json:"rds_instance_id"`
	RDSEndpoint   string `json:"rds_endpoint"`
	RDSPassword   string `json:"rds_password"`

	// Step 3 — EC2
	EC2InstanceID string `json:"ec2_instance_id"`
	EC2PublicIP   string `json:"ec2_public_ip"`

	// Step 4 — S3
	S3BucketName string `json:"s3_bucket_name"`

	// Step 5 — Identity
	SPIREEntryIDs          []string `json:"spire_entry_ids"`
	SVIDExchangePolicyName string   `json:"svid_exchange_policy_name"`

	// Step 6 — Config (local .env path written by provisioner)
	LocalEnvPath string `json:"local_env_path"`

	// FailAtHealth injects a failure in step 7 health validation for demo moment 2.
	FailAtHealth bool `json:"fail_at_health"`
}
