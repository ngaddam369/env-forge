package steps_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/steps"
)

// newRDSClient returns an *rds.Client backed by a mock HTTP transport.
func newRDSClient(rt http.RoundTripper) *rds.Client {
	cfg := aws.Config{
		Region:      "us-east-1",
		HTTPClient:  &http.Client{Transport: rt},
		Credentials: aws.AnonymousCredentials{},
	}
	return rds.NewFromConfig(cfg)
}

// TestRDSStep_IsAlreadyDone covers all branches of IsAlreadyDone, including
// the fix at rds.go:150 that distinguishes DBInstanceNotFound from real errors.
func TestRDSStep_IsAlreadyDone(t *testing.T) {
	authErrXML := `<ErrorResponse>
  <Error>
    <Code>AuthFailure</Code>
    <Message>AWS was not able to validate the provided credentials</Message>
  </Error>
  <RequestId>test-req-id</RequestId>
</ErrorResponse>`

	notFoundXML := `<ErrorResponse>
  <Error>
    <Code>DBInstanceNotFound</Code>
    <Message>DBInstance db-test not found</Message>
  </Error>
  <RequestId>test-req-id</RequestId>
</ErrorResponse>`

	tests := []struct {
		name        string
		env         *environment.Environment
		rt          http.RoundTripper // nil → no AWS call expected
		wantDone    bool
		wantErr     bool
		wantErrText string
	}{
		{
			name:     "empty RDSInstanceID returns false without calling AWS",
			env:      &environment.Environment{ID: "rds-test-0001"},
			wantDone: false,
		},
		{
			name:     "dry-run with InstanceID returns true",
			env:      &environment.Environment{ID: "rds-test-0002", DryRun: true, RDSInstanceID: "db-dryrun"},
			wantDone: true,
		},
		{
			name:     "dry-run without InstanceID returns false",
			env:      &environment.Environment{ID: "rds-test-0003", DryRun: true},
			wantDone: false,
		},
		{
			name:        "non-NotFound AWS error is propagated",
			env:         &environment.Environment{ID: "rds-test-0004", RDSInstanceID: "db-test"},
			rt:          &mockRoundTripper{statusCode: 401, body: authErrXML},
			wantErr:     true,
			wantErrText: "describe db instances",
		},
		{
			name:     "DBInstanceNotFound returns false without error",
			env:      &environment.Environment{ID: "rds-test-0005", RDSInstanceID: "db-test"},
			rt:       &mockRoundTripper{statusCode: 404, body: notFoundXML},
			wantDone: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var step *steps.RDSStep
			if tc.rt != nil {
				step = steps.NewRDSStep(newRDSClient(tc.rt))
			} else {
				step = steps.NewRDSStep(nil)
			}

			done, err := step.IsAlreadyDone(context.Background(), tc.env)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.wantErrText != "" && !strings.Contains(err.Error(), tc.wantErrText) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if done != tc.wantDone {
				t.Errorf("done=%v, want %v", done, tc.wantDone)
			}
		})
	}
}
