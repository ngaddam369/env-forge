package steps_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/ngaddam369/env-forge/internal/environment"
	"github.com/ngaddam369/env-forge/internal/steps"
)

// mockRoundTripper returns a fixed HTTP response for every request.
type mockRoundTripper struct {
	statusCode int
	body       string
}

func (m *mockRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: m.statusCode,
		Body:       io.NopCloser(strings.NewReader(m.body)),
		Header:     make(http.Header),
	}, nil
}

// newEC2Client returns an *ec2.Client backed by a mock HTTP transport.
func newEC2Client(rt http.RoundTripper) *ec2.Client {
	cfg := aws.Config{
		Region:      "us-east-1",
		HTTPClient:  &http.Client{Transport: rt},
		Credentials: aws.AnonymousCredentials{},
	}
	return ec2.NewFromConfig(cfg)
}

// TestEC2Step_Execute_NonDryRun covers the non-dry-run Execute path via mock
// AWS responses. The dry-run happy path is covered by steps_test.go.
func TestEC2Step_Execute_NonDryRun(t *testing.T) {
	// RunInstances XML with an empty instancesSet — guards the bounds check at ec2.go:68.
	emptyInstancesXML := `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>test-req-id</requestId>
  <reservationId>r-test</reservationId>
  <ownerId>123456789012</ownerId>
  <groupSet/>
  <instancesSet/>
</RunInstancesResponse>`

	tests := []struct {
		name        string
		rt          http.RoundTripper
		wantErr     bool
		wantErrText string
	}{
		{
			name:        "empty instance list returns error not panic",
			rt:          &mockRoundTripper{statusCode: 200, body: emptyInstancesXML},
			wantErr:     true,
			wantErrText: "empty instance list",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newEC2Client(tc.rt)
			store := newTestStore(t)
			env := &environment.Environment{
				ID:              "00000000-0000-0000-0000-000000000001",
				Owner:           "test",
				Status:          environment.StatusProvisioning,
				PublicSubnetID:  "subnet-test",
				SecurityGroupID: "sg-test",
			}
			_ = store.Put(env)

			step := steps.NewEC2Step(client)
			err := step.Execute(context.Background(), env, store)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.wantErrText != "" && !strings.Contains(err.Error(), tc.wantErrText) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrText)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// TestEC2Step_IsAlreadyDone covers all branches of the non-dry-run IsAlreadyDone
// path, including the error-propagation fix at ec2.go:141.
func TestEC2Step_IsAlreadyDone(t *testing.T) {
	errorXML := `<Response>
  <Errors>
    <Error>
      <Code>RequestExpired</Code>
      <Message>Request has expired</Message>
    </Error>
  </Errors>
  <RequestID>test-req-id</RequestID>
</Response>`

	emptyReservationsXML := `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>test-req-id</requestId>
  <reservationSet/>
</DescribeInstancesResponse>`

	tests := []struct {
		name        string
		env         *environment.Environment
		rt          http.RoundTripper // nil → no AWS call expected
		wantDone    bool
		wantErr     bool
		wantErrText string
	}{
		{
			name:     "empty EC2InstanceID returns false without calling AWS",
			env:      &environment.Environment{ID: "ec2-test-0001"},
			wantDone: false,
		},
		{
			name:     "dry-run with InstanceID set returns true",
			env:      &environment.Environment{ID: "ec2-test-0002", DryRun: true, EC2InstanceID: "i-dryrun"},
			wantDone: true,
		},
		{
			name:     "dry-run without InstanceID returns false",
			env:      &environment.Environment{ID: "ec2-test-0003", DryRun: true},
			wantDone: false,
		},
		{
			name:        "AWS error is propagated (not silenced)",
			env:         &environment.Environment{ID: "ec2-test-0004", EC2InstanceID: "i-test"},
			rt:          &mockRoundTripper{statusCode: 400, body: errorXML},
			wantErr:     true,
			wantErrText: "describe instances",
		},
		{
			name:     "empty reservations returns false without error",
			env:      &environment.Environment{ID: "ec2-test-0005", EC2InstanceID: "i-gone"},
			rt:       &mockRoundTripper{statusCode: 200, body: emptyReservationsXML},
			wantDone: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var step *steps.EC2Step
			if tc.rt != nil {
				step = steps.NewEC2Step(newEC2Client(tc.rt))
			} else {
				// nil client — dry-run or empty-ID path must not reach AWS.
				step = steps.NewEC2Step(nil)
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
