package steps

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/ngaddam369/env-forge/internal/environment"
)

// S3Step creates a versioning-enabled S3 bucket and writes a placeholder
// config.json. The actual config content is filled in by Step 6 (ConfigStep).
type S3Step struct {
	s3Client *s3.Client
	region   string
}

// NewS3Step creates an S3Step. Pass nil for s3Client to use dry-run mode.
func NewS3Step(client *s3.Client, region string) *S3Step {
	return &S3Step{s3Client: client, region: region}
}

func (s *S3Step) Name() string { return "s3" }

func (s *S3Step) Execute(ctx context.Context, env *environment.Environment, store *environment.Store) error {
	if env.DryRun {
		time.Sleep(1 * time.Second)
		env.S3BucketName = "env-forge-dryrun-" + env.ID[:8]
		return store.Put(env)
	}

	bucket := "env-forge-" + env.ID[:8]
	env.S3BucketName = bucket

	// CreateBucket requires LocationConstraint for all regions except us-east-1.
	input := &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}
	if s.region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(s.region),
		}
	}

	if _, err := s.s3Client.CreateBucket(ctx, input); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}

	// Enable versioning.
	if _, err := s.s3Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		return fmt.Errorf("enable versioning: %w", err)
	}

	// Write placeholder config.json.
	if _, err := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String("config.json"),
		Body:        bytes.NewReader([]byte(`{}`)),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return fmt.Errorf("put placeholder config: %w", err)
	}

	return store.Put(env)
}

func (s *S3Step) Compensate(ctx context.Context, env *environment.Environment, store *environment.Store) error {
	if env.DryRun {
		time.Sleep(1 * time.Second)
		env.S3BucketName = ""
		return store.Put(env)
	}

	if env.S3BucketName == "" {
		return nil
	}

	// Delete all object versions before deleting the bucket.
	if err := s.deleteAllVersions(ctx, env.S3BucketName); err != nil {
		return err
	}

	if _, err := s.s3Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(env.S3BucketName),
	}); err != nil {
		return fmt.Errorf("delete bucket: %w", err)
	}

	env.S3BucketName = ""
	return store.Put(env)
}

func (s *S3Step) deleteAllVersions(ctx context.Context, bucket string) error {
	paginator := s3.NewListObjectVersionsPaginator(s.s3Client, &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list object versions: %w", err)
		}
		var toDelete []s3types.ObjectIdentifier
		for _, v := range page.Versions {
			toDelete = append(toDelete, s3types.ObjectIdentifier{
				Key: v.Key, VersionId: v.VersionId,
			})
		}
		for _, dm := range page.DeleteMarkers {
			toDelete = append(toDelete, s3types.ObjectIdentifier{
				Key: dm.Key, VersionId: dm.VersionId,
			})
		}
		if len(toDelete) == 0 {
			continue
		}
		if _, err := s.s3Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: toDelete, Quiet: aws.Bool(true)},
		}); err != nil {
			return fmt.Errorf("delete objects: %w", err)
		}
	}
	return nil
}

func (s *S3Step) IsAlreadyDone(_ context.Context, env *environment.Environment) (bool, error) {
	return env.S3BucketName != "", nil
}
