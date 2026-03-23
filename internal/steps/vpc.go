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

// VPCStep provisions a VPC, public + private subnets, and a security group that
// allows Postgres (5432) from within the VPC. All resources are tagged with the
// env-id for easy identification and cleanup.
type VPCStep struct {
	ec2Client *ec2.Client
}

// NewVPCStep creates a VPCStep. Pass nil for ec2Client to use dry-run mode.
func NewVPCStep(client *ec2.Client) *VPCStep {
	return &VPCStep{ec2Client: client}
}

func (s *VPCStep) Name() string { return "vpc" }

func (s *VPCStep) Execute(ctx context.Context, env *environment.Environment, store *environment.Store) error {
	if env.DryRun {
		time.Sleep(2 * time.Second)
		env.VPCID = "vpc-dryrun-" + env.ID[:8]
		env.PublicSubnetID = "subnet-pub-" + env.ID[:8]
		env.PrivateSubnetID = "subnet-prv-" + env.ID[:8]
		env.SecurityGroupID = "sg-" + env.ID[:8]
		return store.Put(env)
	}

	tags := []ec2types.Tag{
		{Key: aws.String("infra-provisioner"), Value: aws.String("true")},
		{Key: aws.String("env-id"), Value: aws.String(env.ID)},
		{Key: aws.String("Name"), Value: aws.String("env-forge-" + env.ID[:8])},
	}
	tagSpec := func(rt ec2types.ResourceType) []ec2types.TagSpecification {
		return []ec2types.TagSpecification{{ResourceType: rt, Tags: tags}}
	}

	// Create VPC.
	vpcOut, err := s.ec2Client.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String("10.0.0.0/16"),
		TagSpecifications: tagSpec(ec2types.ResourceTypeVpc),
	})
	if err != nil {
		return fmt.Errorf("create vpc: %w", err)
	}
	env.VPCID = aws.ToString(vpcOut.Vpc.VpcId)

	// Enable DNS hostnames (required for RDS).
	if _, err := s.ec2Client.ModifyVpcAttribute(ctx, &ec2.ModifyVpcAttributeInput{
		VpcId:              vpcOut.Vpc.VpcId,
		EnableDnsHostnames: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
	}); err != nil {
		return fmt.Errorf("enable dns hostnames: %w", err)
	}

	// Poll until VPC is available.
	waiter := ec2.NewVpcAvailableWaiter(s.ec2Client)
	if err := waiter.Wait(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{env.VPCID},
	}, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for vpc available: %w", err)
	}

	// Public subnet.
	pubOut, err := s.ec2Client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(env.VPCID),
		CidrBlock:         aws.String("10.0.1.0/24"),
		TagSpecifications: tagSpec(ec2types.ResourceTypeSubnet),
	})
	if err != nil {
		return fmt.Errorf("create public subnet: %w", err)
	}
	env.PublicSubnetID = aws.ToString(pubOut.Subnet.SubnetId)

	// Private subnet.
	prvOut, err := s.ec2Client.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(env.VPCID),
		CidrBlock:         aws.String("10.0.2.0/24"),
		TagSpecifications: tagSpec(ec2types.ResourceTypeSubnet),
	})
	if err != nil {
		return fmt.Errorf("create private subnet: %w", err)
	}
	env.PrivateSubnetID = aws.ToString(prvOut.Subnet.SubnetId)

	// Security group.
	sgOut, err := s.ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:         aws.String("env-forge-" + env.ID[:8]),
		Description:       aws.String("env-forge environment " + env.ID),
		VpcId:             aws.String(env.VPCID),
		TagSpecifications: tagSpec(ec2types.ResourceTypeSecurityGroup),
	})
	if err != nil {
		return fmt.Errorf("create security group: %w", err)
	}
	env.SecurityGroupID = aws.ToString(sgOut.GroupId)

	// Allow Postgres from within the VPC (10.0.0.0/16).
	if _, err := s.ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(env.SecurityGroupID),
		IpPermissions: []ec2types.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int32(5432),
			ToPort:     aws.Int32(5432),
			IpRanges:   []ec2types.IpRange{{CidrIp: aws.String("10.0.0.0/16")}},
		}},
	}); err != nil {
		return fmt.Errorf("authorize postgres ingress: %w", err)
	}

	return store.Put(env)
}

func (s *VPCStep) Compensate(ctx context.Context, env *environment.Environment, store *environment.Store) error {
	if env.DryRun {
		time.Sleep(1 * time.Second)
		env.SecurityGroupID = ""
		env.PublicSubnetID = ""
		env.PrivateSubnetID = ""
		env.VPCID = ""
		return store.Put(env)
	}

	// Delete in reverse dependency order: SG → subnets → VPC.
	if env.SecurityGroupID != "" {
		if _, err := s.ec2Client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(env.SecurityGroupID),
		}); err != nil {
			return fmt.Errorf("delete security group: %w", err)
		}
	}
	for _, subnetID := range []string{env.PublicSubnetID, env.PrivateSubnetID} {
		if subnetID == "" {
			continue
		}
		if _, err := s.ec2Client.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
			SubnetId: aws.String(subnetID),
		}); err != nil {
			return fmt.Errorf("delete subnet %s: %w", subnetID, err)
		}
	}
	if env.VPCID != "" {
		if _, err := s.ec2Client.DeleteVpc(ctx, &ec2.DeleteVpcInput{
			VpcId: aws.String(env.VPCID),
		}); err != nil {
			return fmt.Errorf("delete vpc: %w", err)
		}
	}

	env.SecurityGroupID = ""
	env.PublicSubnetID = ""
	env.PrivateSubnetID = ""
	env.VPCID = ""
	return store.Put(env)
}

func (s *VPCStep) IsAlreadyDone(_ context.Context, env *environment.Environment) (bool, error) {
	return env.VPCID != "", nil
}
