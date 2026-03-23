package conductor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ngaddam369/env-forge/internal/environment"
	client "github.com/ngaddam369/saga-conductor/pkg/client"
	pb "github.com/ngaddam369/saga-conductor/proto/saga/v1"
)

// payload is the opaque JSON body sent by saga-conductor to every step endpoint.
// Kept minimal — all provisioning state lives in BoltDB, keyed by env_id.
type payload struct {
	EnvID string `json:"env_id"`
}

// Client wraps the saga-conductor gRPC client with env-forge–specific saga setup.
type Client struct {
	inner   *client.Client
	selfURL string // base URL of this provisioner's step HTTP server
}

// New creates a conductor Client.
//
//	conductorAddr: gRPC address of saga-conductor (e.g. "localhost:8080")
//	selfURL:       HTTP base URL of this provisioner's step server
//	               (e.g. "http://env-forge.default.svc.cluster.local:9090")
func New(conductorAddr, selfURL string) (*Client, error) {
	c, err := client.New(conductorAddr, client.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("dial saga-conductor: %w", err)
	}
	return &Client{inner: c, selfURL: selfURL}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.inner.Close()
}

// Provision creates and starts a saga for the given environment, blocking until
// the saga reaches a terminal state. Returns the final SagaExecution.
func (c *Client) Provision(ctx context.Context, env *environment.Environment) (*pb.SagaExecution, error) {
	payloadBytes, err := json.Marshal(payload{EnvID: env.ID})
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	// Create saga with all 8 step definitions.
	createResp, err := c.inner.CreateSaga(ctx, &pb.CreateSagaRequest{
		Name:    "provision-env-" + env.ID[:8],
		Steps:   c.buildStepDefs(),
		Payload: payloadBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("create saga: %w", err)
	}

	// Start saga — blocks until terminal state.
	exec, err := c.inner.StartSaga(ctx, createResp.Id)
	if err != nil {
		return nil, fmt.Errorf("start saga: %w", err)
	}
	return exec, nil
}

// GetSaga returns the current saga execution state.
func (c *Client) GetSaga(ctx context.Context, sagaID string) (*pb.SagaExecution, error) {
	return c.inner.GetSaga(ctx, sagaID)
}

// buildStepDefs returns the 8 StepDefinitions with URLs pointing at this
// provisioner's HTTP step server. Timeout is generous (10 min) to accommodate
// RDS and EC2 wait times.
func (c *Client) buildStepDefs() []*pb.StepDefinition {
	type stepSpec struct {
		name    string
		fwd     string
		comp    string
		timeout int32
	}
	specs := []stepSpec{
		{"vpc", "/steps/vpc", "/steps/vpc/compensate", 120},
		{"rds", "/steps/rds", "/steps/rds/compensate", 600},
		{"ec2", "/steps/ec2", "/steps/ec2/compensate", 300},
		{"s3", "/steps/s3", "/steps/s3/compensate", 60},
		{"identity", "/steps/identity", "/steps/identity/compensate", 60},
		{"config", "/steps/config", "/steps/config/compensate", 60},
		{"health", "/steps/health", "", 60},
		{"registry", "/steps/registry", "/steps/registry/compensate", 30},
	}

	defs := make([]*pb.StepDefinition, len(specs))
	for i, sp := range specs {
		defs[i] = &pb.StepDefinition{
			Name:           sp.name,
			ForwardUrl:     c.selfURL + sp.fwd,
			TimeoutSeconds: sp.timeout,
			MaxRetries:     3,
			RetryBackoffMs: 1000,
		}
		if sp.comp != "" {
			defs[i].CompensateUrl = c.selfURL + sp.comp
		}
	}
	return defs
}
