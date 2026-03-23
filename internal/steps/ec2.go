package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/ngaddam369/env-forge/internal/environment"
)

// amazon linux 2023 AMI (us-east-1). In production this should be resolved
// dynamically via SSM /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64.
const amazonLinux2023AMI = "ami-0c101f26f147fa7fd"

// EC2Step launches a t3.micro Amazon Linux 2023 instance in the public subnet.
// User data installs the SPIRE agent on startup.
type EC2Step struct {
	ec2Client *ec2.Client
}

// NewEC2Step creates an EC2Step. Pass nil for ec2Client to use dry-run mode.
func NewEC2Step(client *ec2.Client) *EC2Step {
	return &EC2Step{ec2Client: client}
}

func (s *EC2Step) Name() string { return "ec2" }

func (s *EC2Step) Execute(ctx context.Context, env *environment.Environment, store environment.StateWriter) error {
	if env.DryRun {
		time.Sleep(3 * time.Second)
		env.EC2InstanceID = "i-dryrun-" + env.ID[:8]
		env.EC2PublicIP = "1.2.3.4"
		return store.Put(env)
	}

	tags := []ec2types.Tag{
		{Key: aws.String("infra-provisioner"), Value: aws.String("true")},
		{Key: aws.String("env-id"), Value: aws.String(env.ID)},
		{Key: aws.String("Name"), Value: aws.String("env-forge-" + env.ID[:8])},
	}

	// User data installs SPIRE agent on startup.
	userData := `#!/bin/bash
yum install -y curl
curl -sSfL https://github.com/spiffe/spire/releases/download/v1.9.6/spire-1.9.6-linux-x86_64-glibc.tar.gz | tar xz -C /opt
ln -s /opt/spire-1.9.6/bin/spire-agent /usr/local/bin/spire-agent
`

	out, err := s.ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:          aws.String(amazonLinux2023AMI),
		InstanceType:     ec2types.InstanceTypeT3Micro,
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		SubnetId:         aws.String(env.PublicSubnetID),
		SecurityGroupIds: []string{env.SecurityGroupID},
		UserData:         aws.String(userData),
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
		},
	})
	if err != nil {
		return fmt.Errorf("run instances: %w", err)
	}

	env.EC2InstanceID = aws.ToString(out.Instances[0].InstanceId)

	// Save before polling.
	if err := store.Put(env); err != nil {
		return err
	}

	// Poll until running (~90 seconds).
	waiter := ec2.NewInstanceRunningWaiter(s.ec2Client)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{env.EC2InstanceID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for instance running: %w", err)
	}

	// Retrieve public IP.
	desc, err := s.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{env.EC2InstanceID},
	})
	if err != nil {
		return fmt.Errorf("describe instances: %w", err)
	}
	if len(desc.Reservations) > 0 && len(desc.Reservations[0].Instances) > 0 {
		env.EC2PublicIP = aws.ToString(desc.Reservations[0].Instances[0].PublicIpAddress)
	}

	return store.Put(env)
}

func (s *EC2Step) Compensate(ctx context.Context, env *environment.Environment, store environment.StateWriter) error {
	if env.DryRun {
		time.Sleep(1 * time.Second)
		env.EC2InstanceID = ""
		env.EC2PublicIP = ""
		return store.Put(env)
	}

	if env.EC2InstanceID == "" {
		return nil
	}

	if _, err := s.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{env.EC2InstanceID},
	}); err != nil {
		return fmt.Errorf("terminate instances: %w", err)
	}

	waiter := ec2.NewInstanceTerminatedWaiter(s.ec2Client)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{env.EC2InstanceID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for instance terminated: %w", err)
	}

	env.EC2InstanceID = ""
	env.EC2PublicIP = ""
	return store.Put(env)
}

func (s *EC2Step) IsAlreadyDone(ctx context.Context, env *environment.Environment) (bool, error) {
	if env.DryRun {
		return env.EC2InstanceID != "", nil
	}
	if env.EC2InstanceID == "" {
		return false, nil
	}
	out, err := s.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{env.EC2InstanceID},
	})
	if err != nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return false, nil //nolint:nilerr
	}
	state := out.Reservations[0].Instances[0].State
	if state == nil {
		return false, nil
	}
	return state.Name == ec2types.InstanceStateNameRunning, nil
}
