package account

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bcmdataexports"
	etypes "github.com/aws/aws-sdk-go-v2/service/bcmdataexports/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	stypes "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// ExportQuery is the CUR 2.0 query: only the columns the reports need.
const ExportQuery = "SELECT bill_billing_entity, bill_billing_period_start_date, line_item_usage_account_id, " +
	"line_item_usage_start_date, line_item_product_code, line_item_usage_type, line_item_operation, " +
	"line_item_iam_principal, line_item_unblended_cost, product, tags FROM COST_AND_USAGE_REPORT"

var exportTable = map[string]map[string]string{"COST_AND_USAGE_REPORT": {
	"TIME_GRANULARITY":                      "HOURLY",
	"INCLUDE_IAM_PRINCIPAL_DATA":            "TRUE",
	"INCLUDE_RESOURCES":                     "FALSE",
	"INCLUDE_SPLIT_COST_ALLOCATION_DATA":    "FALSE",
	"INCLUDE_MANUAL_DISCOUNT_COMPATIBILITY": "FALSE",
	"INCLUDE_CAPACITY_RESERVATION_DATA":     "FALSE",
}}

func (e *Env) costExport(ctx context.Context) ([]*plan.Item, error) {
	b, err := e.bucket(ctx)
	if err != nil {
		return nil, err
	}
	x, err := e.export(ctx)
	if err != nil {
		return nil, err
	}
	return []*plan.Item{b, x}, nil
}

// BucketID returns the bucket's bedrock-admin:id and whether the bucket exists.
func BucketID(ctx context.Context, c *s3.Client, bucket string) (string, bool, error) {
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		if awsx.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("checking bucket %s: %w", bucket, err)
	}
	t, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	if err != nil {
		if awsx.IsNotFound(err) {
			return "", true, nil
		}
		return "", true, fmt.Errorf("reading tags of bucket %s: %w", bucket, err)
	}
	for _, tag := range t.TagSet {
		if aws.ToString(tag.Key) == policy.IDTag {
			return aws.ToString(tag.Value), true, nil
		}
	}
	return "", true, nil
}

func (e *Env) bucket(ctx context.Context) (*plan.Item, error) {
	bucket := e.Cfg.CostExport.Bucket
	it := e.item("1.1", "cost export bucket "+bucket)
	id, exists, err := BucketID(ctx, e.C.S3, bucket)
	if err != nil {
		if awsx.ErrorCode(err) == "Forbidden" || awsx.IsAccessDenied(err) {
			it.Op, it.Status = plan.Conflict, "the name is taken by a bucket you can't read (another account?)"
			return it, nil
		}
		return nil, err
	}
	pol := policy.CurBucket(bucket, e.C.Account)
	if !exists {
		it.Op, it.Details = plan.Create, []string{"create (private, writable only by Data Exports)"}
		it.Apply = func(ctx context.Context) error {
			in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
			if e.Cfg.Region != "us-east-1" {
				in.CreateBucketConfiguration = &stypes.CreateBucketConfiguration{
					LocationConstraint: stypes.BucketLocationConstraint(e.Cfg.Region)}
			}
			if _, err := e.C.S3.CreateBucket(ctx, in); err != nil {
				return fmt.Errorf("creating bucket %s: %w", bucket, err)
			}
			return e.fixBucket(ctx, bucket, pol, true, true, true)
		}
		return it, nil
	}
	if !e.owned(it, id) {
		return it, nil
	}
	var fixPolicy, fixPAB bool
	p, err := e.C.S3.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	switch {
	case awsx.IsNotFound(err):
		fixPolicy = true
		it.Details = append(it.Details, "bucket policy missing")
	case err != nil:
		return nil, fmt.Errorf("reading the policy of bucket %s: %w", bucket, err)
	case !policy.Equal(aws.ToString(p.Policy), pol):
		fixPolicy = true
		it.Details = append(it.Details, "bucket policy changed outside the file")
	}
	pab, err := e.C.S3.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String(bucket)})
	if err != nil && !awsx.IsNotFound(err) {
		return nil, fmt.Errorf("reading the public access block of %s: %w", bucket, err)
	}
	if err != nil || !allBlocked(pab.PublicAccessBlockConfiguration) {
		fixPAB = true
		it.Details = append(it.Details, "public access not fully blocked")
	}
	if fixPolicy || fixPAB {
		it.Op = plan.Update
		it.Apply = func(ctx context.Context) error { return e.fixBucket(ctx, bucket, pol, fixPolicy, fixPAB, false) }
	}
	return it, nil
}

func allBlocked(c *stypes.PublicAccessBlockConfiguration) bool {
	return c != nil && aws.ToBool(c.BlockPublicAcls) && aws.ToBool(c.IgnorePublicAcls) &&
		aws.ToBool(c.BlockPublicPolicy) && aws.ToBool(c.RestrictPublicBuckets)
}

func (e *Env) fixBucket(ctx context.Context, bucket, pol string, fixPolicy, fixPAB, tag bool) error {
	if fixPAB {
		if _, err := e.C.S3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{Bucket: aws.String(bucket),
			PublicAccessBlockConfiguration: &stypes.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true),
				IgnorePublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true)}}); err != nil {
			return fmt.Errorf("blocking public access to %s: %w", bucket, err)
		}
	}
	if fixPolicy {
		if _, err := e.C.S3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), Policy: aws.String(pol)}); err != nil {
			return fmt.Errorf("setting the policy of %s: %w", bucket, err)
		}
	}
	if tag {
		if _, err := e.C.S3.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(bucket),
			Tagging: &stypes.Tagging{TagSet: []stypes.Tag{{Key: aws.String(policy.IDTag), Value: aws.String(e.Cfg.ID)}}}}); err != nil {
			return fmt.Errorf("tagging %s: %w", bucket, err)
		}
	}
	return nil
}

func (e *Env) wantExport() *etypes.Export {
	return &etypes.Export{
		Name:        aws.String(e.Cfg.CostExport.Name),
		Description: aws.String("CUR 2.0 with IAM principal data, for per-person Bedrock reporting (bedrock-admin)"),
		DataQuery:   &etypes.DataQuery{QueryStatement: aws.String(ExportQuery), TableConfigurations: exportTable},
		DestinationConfigurations: &etypes.DestinationConfigurations{S3Destination: &etypes.S3Destination{
			S3Bucket: aws.String(e.Cfg.CostExport.Bucket), S3Prefix: aws.String(e.Cfg.CostExport.Prefix),
			S3Region: aws.String(e.Cfg.Region),
			S3OutputConfigurations: &etypes.S3OutputConfigurations{OutputType: etypes.S3OutputTypeCustom,
				Format: etypes.FormatOptionParquet, Compression: etypes.CompressionOptionParquet,
				Overwrite: etypes.OverwriteOptionOverwriteReport}}},
		RefreshCadence: &etypes.RefreshCadence{Frequency: etypes.FrequencyOptionSynchronous},
	}
}

// FindExport returns the ARN of the export with this name, or "".
func FindExport(ctx context.Context, c *bcmdataexports.Client, name string) (string, error) {
	p := bcmdataexports.NewListExportsPaginator(c, &bcmdataexports.ListExportsInput{})
	for p.HasMorePages() {
		r, err := p.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("listing cost exports: %w", err)
		}
		for _, x := range r.Exports {
			if aws.ToString(x.ExportName) == name {
				return aws.ToString(x.ExportArn), nil
			}
		}
	}
	return "", nil
}

// ExportID returns the bedrock-admin:id tag of an export.
func ExportID(ctx context.Context, c *bcmdataexports.Client, arn string) (string, error) {
	r, err := c.ListTagsForResource(ctx, &bcmdataexports.ListTagsForResourceInput{ResourceArn: aws.String(arn)})
	if err != nil {
		return "", fmt.Errorf("reading tags of %s: %w", arn, err)
	}
	for _, t := range r.ResourceTags {
		if aws.ToString(t.Key) == policy.IDTag {
			return aws.ToString(t.Value), nil
		}
	}
	return "", nil
}

func (e *Env) export(ctx context.Context) (*plan.Item, error) {
	name := e.Cfg.CostExport.Name
	it := e.item("1.1", "cost export "+name)
	want := e.wantExport()
	arn, err := FindExport(ctx, e.C.Exports, name)
	if err != nil {
		return nil, err
	}
	if arn == "" {
		it.Op = plan.Create
		it.Details = []string{fmt.Sprintf("create (CUR 2.0, Parquet, to s3://%s/%s)", e.Cfg.CostExport.Bucket, e.Cfg.ExportPrefix())}
		it.Apply = func(ctx context.Context) error {
			_, err := e.C.Exports.CreateExport(ctx, &bcmdataexports.CreateExportInput{Export: want,
				ResourceTags: []etypes.ResourceTag{{Key: aws.String(policy.IDTag), Value: aws.String(e.Cfg.ID)}}})
			if err != nil {
				return fmt.Errorf("creating cost export %s: %w", name, err)
			}
			return nil
		}
		return it, nil
	}
	id, err := ExportID(ctx, e.C.Exports, arn)
	if err != nil {
		return nil, err
	}
	if !e.owned(it, id) {
		return it, nil
	}
	got, err := e.C.Exports.GetExport(ctx, &bcmdataexports.GetExportInput{ExportArn: aws.String(arn)})
	if err != nil {
		return nil, fmt.Errorf("reading cost export %s: %w", name, err)
	}
	if d := exportDiff(got.Export, want); len(d) > 0 {
		it.Op, it.Details = plan.Update, d
		it.Apply = func(ctx context.Context) error {
			w := *want
			w.ExportArn = aws.String(arn)
			if _, err := e.C.Exports.UpdateExport(ctx, &bcmdataexports.UpdateExportInput{ExportArn: aws.String(arn), Export: &w}); err != nil {
				return fmt.Errorf("updating cost export %s: %w", name, err)
			}
			return nil
		}
	}
	return it, nil
}

func exportDiff(got, want *etypes.Export) []string {
	var d []string
	if got == nil || got.DestinationConfigurations == nil || got.DestinationConfigurations.S3Destination == nil {
		return []string{"destination missing"}
	}
	g, w := got.DestinationConfigurations.S3Destination, want.DestinationConfigurations.S3Destination
	for _, f := range []struct{ name, got, want string }{
		{"bucket", aws.ToString(g.S3Bucket), aws.ToString(w.S3Bucket)},
		{"prefix", aws.ToString(g.S3Prefix), aws.ToString(w.S3Prefix)},
		{"bucket region", aws.ToString(g.S3Region), aws.ToString(w.S3Region)},
	} {
		if f.got != f.want {
			d = append(d, fmt.Sprintf("%s %s → %s", f.name, f.got, f.want))
		}
	}
	if o := g.S3OutputConfigurations; o == nil || o.Format != etypes.FormatOptionParquet || o.Overwrite != etypes.OverwriteOptionOverwriteReport {
		d = append(d, "output format changed outside the file")
	}
	if got.DataQuery == nil || normalizeSQL(aws.ToString(got.DataQuery.QueryStatement)) != normalizeSQL(ExportQuery) {
		d = append(d, "columns changed outside the file")
	} else if t := got.DataQuery.TableConfigurations["COST_AND_USAGE_REPORT"]; t["INCLUDE_IAM_PRINCIPAL_DATA"] != "TRUE" {
		d = append(d, "IAM principal data not included")
	}
	return d
}

func normalizeSQL(s string) string { return strings.Join(strings.Fields(strings.ToUpper(s)), " ") }
