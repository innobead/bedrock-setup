// Package awsx builds the AWS SDK clients the CLIs use.
package awsx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bcmdataexports"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// Clients holds one client per service, all for the same credentials.
type Clients struct {
	Cfg       aws.Config
	Account   string
	CallerArn string
	Region    string

	IAM       *iam.Client
	STS       *sts.Client
	S3        *s3.Client
	Budgets   *budgets.Client
	Exports   *bcmdataexports.Client
	Lambda    *lambda.Client
	Scheduler *scheduler.Client
	Bedrock   *bedrock.Client
	Runtime   *bedrockruntime.Client
	CE        *costexplorer.Client
}

// Load reads AWS credentials for profile ("" for the default chain) and region.
func Load(ctx context.Context, profile, region string) (aws.Config, error) {
	// A stalled connection otherwise blocks a call for minutes; with a timeout the SDK retries it.
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.ResponseHeaderTimeout = 60 * time.Second
	})
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithHTTPClient(httpClient)}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// New loads credentials and checks who they belong to. When account is not empty, it must match.
func New(ctx context.Context, profile, region, account string) (*Clients, error) {
	cfg, err := Load(ctx, profile, region)
	if err != nil {
		return nil, fmt.Errorf("loading AWS credentials: %w", err)
	}
	c := FromConfig(cfg)
	id, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		hint := ""
		if profile != "" {
			hint = fmt.Sprintf(" (try: aws sso login --profile %s)", profile)
		}
		return nil, fmt.Errorf("AWS credentials don't work%s: %w", hint, err)
	}
	c.Account, c.CallerArn = aws.ToString(id.Account), aws.ToString(id.Arn)
	if account != "" && c.Account != account {
		return nil, fmt.Errorf("these credentials are for account %s, but the config is for %s", c.Account, account)
	}
	return c, nil
}

// QuietS3 stops the SDK logging "Response has no supported checksum" for ranged GETs.
func QuietS3(o *s3.Options) { o.DisableLogOutputChecksumValidationSkipped = true }

// FromConfig creates every client. Data Exports and Cost Explorer are always called in us-east-1.
func FromConfig(cfg aws.Config) *Clients {
	east := cfg.Copy()
	east.Region = "us-east-1"
	return &Clients{
		Cfg:       cfg,
		Region:    cfg.Region,
		IAM:       iam.NewFromConfig(cfg),
		STS:       sts.NewFromConfig(cfg),
		S3:        s3.NewFromConfig(cfg, QuietS3),
		Budgets:   budgets.NewFromConfig(cfg),
		Exports:   bcmdataexports.NewFromConfig(east),
		Lambda:    lambda.NewFromConfig(cfg),
		Scheduler: scheduler.NewFromConfig(cfg),
		Bedrock:   bedrock.NewFromConfig(cfg),
		Runtime:   bedrockruntime.NewFromConfig(cfg),
		CE:        costexplorer.NewFromConfig(east),
	}
}

// BucketRegion returns the region of bucket and whether it exists.
func (c *Clients) BucketRegion(ctx context.Context, bucket string) (string, bool, error) {
	loc, err := c.S3.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(bucket)})
	if IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	switch region := string(loc.LocationConstraint); region {
	case "":
		return "us-east-1", true, nil
	case "EU":
		return "eu-west-1", true, nil
	default:
		return region, true, nil
	}
}

// S3In returns an S3 client for region.
func (c *Clients) S3In(region string) *s3.Client {
	if region == "" || region == c.Region {
		return c.S3
	}
	return s3.NewFromConfig(c.Cfg, QuietS3, func(o *s3.Options) { o.Region = region })
}

// BucketS3 returns an S3 client for the bucket's region, or the default client when the region
// can't be read.
func (c *Clients) BucketS3(ctx context.Context, bucket string) *s3.Client {
	region, _, err := c.BucketRegion(ctx, bucket)
	if err != nil {
		return c.S3
	}
	return c.S3In(region)
}

// ErrorCode returns the AWS error code of err, or "".
func ErrorCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// IsNotFound reports whether err means the resource does not exist.
func IsNotFound(err error) bool {
	switch ErrorCode(err) {
	case "NoSuchEntity", "NotFoundException", "NoSuchBucket", "NotFound", "ResourceNotFoundException",
		"NoSuchKey", "NoSuchTagSet", "NoSuchBucketPolicy", "NoSuchPublicAccessBlockConfiguration":
		return true
	}
	return false
}

// IsAccessDenied reports whether err is a permissions error.
func IsAccessDenied(err error) bool {
	c := ErrorCode(err)
	return c == "AccessDenied" || c == "AccessDeniedException" || c == "UnauthorizedOperation" ||
		strings.Contains(c, "AccessDenied")
}

// Identity is the readable form of an STS ARN: the session name for assumed roles, the user name
// for IAM users.
func Identity(arn string) string {
	parts := strings.Split(arn, "/")
	return parts[len(parts)-1]
}
