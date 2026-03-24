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
//	               (e.g. "http://forge-worker.default.svc.cluster.local:9091")
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

// CreateEnvSaga registers a new provisioning saga for the environment and
// returns the saga ID. The saga is in PENDING state and will not execute until
// StartEnvSaga is called.
//
// An idempotency_key derived from the env ID is included so that retried
// invocations (e.g. from a CLI retry) do not create duplicate sagas.
// A saga_timeout_seconds of 300 is set as an overall safety deadline.
func (c *Client) CreateEnvSaga(ctx context.Context, env *environment.Environment) (string, error) {
	payloadBytes, err := json.Marshal(payload{EnvID: env.ID})
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	createResp, err := c.inner.CreateSaga(ctx, &pb.CreateSagaRequest{
		Name:               "provision-env-" + env.ID[:8],
		IdempotencyKey:     "env-" + env.ID,
		SagaTimeoutSeconds: 300,
		Steps:              c.buildStepDefs(),
		Payload:            payloadBytes,
	})
	if err != nil {
		return "", fmt.Errorf("create saga: %w", err)
	}
	return createResp.Id, nil
}

// StartEnvSaga starts the saga identified by sagaID and blocks until it reaches
// a terminal state. Returns the final SagaExecution.
func (c *Client) StartEnvSaga(ctx context.Context, sagaID string) (*pb.SagaExecution, error) {
	exec, err := c.inner.StartSaga(ctx, sagaID)
	if err != nil {
		return nil, fmt.Errorf("start saga: %w", err)
	}
	return exec, nil
}

// GetSaga returns the current saga execution state.
func (c *Client) GetSaga(ctx context.Context, sagaID string) (*pb.SagaExecution, error) {
	return c.inner.GetSaga(ctx, sagaID)
}

// ListSagas returns a page of sagas matching the optional status filter.
// nextPageToken is empty when there are no more pages.
func (c *Client) ListSagas(ctx context.Context, req *pb.ListSagasRequest) ([]*pb.SagaExecution, string, error) {
	return c.inner.ListSagas(ctx, req)
}

// AbortSaga forcibly moves the saga to ABORTED without triggering compensation.
func (c *Client) AbortSaga(ctx context.Context, sagaID string) (*pb.SagaExecution, error) {
	return c.inner.AbortSaga(ctx, sagaID)
}

// buildStepDefs returns the 8 StepDefinitions with URLs pointing at the
// forge-worker step HTTP server. Each step has individually tuned timeout,
// max_retries, and retry_backoff_ms to demonstrate per-step configuration.
//
//   - vpc / s3 / identity / config / registry: fast operations, short timeout
//   - rds / ec2: slow AWS operations, longer timeout + more retries
//   - health: moderate timeout, fewer retries (fast fail on health issues)
func (c *Client) buildStepDefs() []*pb.StepDefinition {
	type stepSpec struct {
		name           string
		fwd            string
		comp           string
		timeoutSeconds int32
		maxRetries     int32
		retryBackoffMs int32
	}
	specs := []stepSpec{
		{"vpc", "/steps/vpc", "/steps/vpc/compensate", 30, 2, 500},
		{"rds", "/steps/rds", "/steps/rds/compensate", 600, 3, 2000},
		{"ec2", "/steps/ec2", "/steps/ec2/compensate", 300, 3, 2000},
		{"s3", "/steps/s3", "/steps/s3/compensate", 30, 2, 500},
		{"identity", "/steps/identity", "/steps/identity/compensate", 30, 2, 500},
		{"config", "/steps/config", "/steps/config/compensate", 30, 2, 500},
		{"health", "/steps/health", "/steps/health/compensate", 60, 1, 1000},
		{"registry", "/steps/registry", "/steps/registry/compensate", 10, 1, 200},
	}

	defs := make([]*pb.StepDefinition, len(specs))
	for i, sp := range specs {
		defs[i] = &pb.StepDefinition{
			Name:           sp.name,
			ForwardUrl:     c.selfURL + sp.fwd,
			CompensateUrl:  c.selfURL + sp.comp,
			TimeoutSeconds: sp.timeoutSeconds,
			MaxRetries:     sp.maxRetries,
			RetryBackoffMs: sp.retryBackoffMs,
		}
	}
	return defs
}
