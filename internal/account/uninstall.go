package account

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bcmdataexports"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	stypes "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/iamrole"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// UserRoles lists the roles under /bedrock-users/ (any id).
func UserRoles(ctx context.Context, c *iam.Client) ([]string, error) {
	var out []string
	p := iam.NewListRolesPaginator(c, &iam.ListRolesInput{PathPrefix: aws.String(policy.UsersPath)})
	for p.HasMorePages() {
		r, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, role := range r.Roles {
			out = append(out, aws.ToString(role.RoleName))
		}
	}
	return out, nil
}

// Uninstall plans removing the account setup. Only resources with this config's id are removed;
// the cost export bucket only with deleteData.
func (e *Env) Uninstall(ctx context.Context, deleteData bool) ([]*plan.Item, error) {
	var out []*plan.Item
	del := func(step, target, id string, f func(context.Context) error) {
		it := e.item(step, target)
		if !e.owned(it, id) {
			if it.Op == plan.Conflict {
				it.Op, it.Status = plan.Info, "left alone (not created by bedrock-admin)"
			} else {
				it.Status = "left alone (managed by " + id + ")"
			}
			out = append(out, it)
			return
		}
		it.Op, it.Details, it.Status, it.Apply = plan.Delete, []string{"remove"}, "", f
		out = append(out, it)
	}
	missing := func(step, target string) {
		it := e.item(step, target)
		it.Op, it.Status = plan.Info, "not present"
		out = append(out, it)
	}

	if s, err := e.C.Scheduler.GetSchedule(ctx, &scheduler.GetScheduleInput{Name: aws.String(policy.ScheduleName)}); err == nil {
		del("1.4", "schedule "+policy.ScheduleName, ScheduleID(aws.ToString(s.Description)), func(ctx context.Context) error {
			_, err := e.C.Scheduler.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{Name: aws.String(policy.ScheduleName)})
			return err
		})
	} else if awsx.IsNotFound(err) {
		missing("1.4", "schedule "+policy.ScheduleName)
	} else {
		return nil, err
	}
	if f, err := e.C.Lambda.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String(policy.LambdaName)}); err == nil {
		del("1.4", "Lambda "+policy.LambdaName, f.Tags[policy.IDTag], func(ctx context.Context) error {
			_, err := e.C.Lambda.DeleteFunction(ctx, &lambda.DeleteFunctionInput{FunctionName: aws.String(policy.LambdaName)})
			return err
		})
	} else if awsx.IsNotFound(err) {
		missing("1.4", "Lambda "+policy.LambdaName)
	} else {
		return nil, err
	}
	for _, r := range []struct{ step, label, name string }{
		{"1.4", "scheduler role ", policy.SchedulerRole},
		{"1.4", "Lambda role ", policy.LambdaRole},
		{"1.3", "budget actions role ", policy.BudgetActionsRole},
	} {
		st, err := iamrole.Observe(ctx, e.C.IAM, iamrole.Spec{Name: r.name, Path: policy.Path})
		if err != nil {
			return nil, err
		}
		if !st.Exists {
			missing(r.step, r.label+r.name)
			continue
		}
		name := r.name
		del(r.step, r.label+name, st.ID(), func(ctx context.Context) error { return iamrole.Delete(ctx, e.C.IAM, name) })
	}
	arn := policy.DenyPolicyArn(e.C.Account)
	if id, ok, err := PolicyID(ctx, e.C.IAM, arn); err != nil {
		return nil, err
	} else if ok {
		del("1.2", "pause policy "+policy.DenyPolicyName, id, func(ctx context.Context) error { return deletePolicy(ctx, e.C.IAM, arn) })
	} else {
		missing("1.2", "pause policy "+policy.DenyPolicyName)
	}
	if x, err := FindExport(ctx, e.C.Exports, e.Cfg.CostExport.Name); err != nil {
		return nil, err
	} else if x != "" {
		id, err := ExportID(ctx, e.C.Exports, x)
		if err != nil {
			return nil, err
		}
		del("1.1", "cost export "+e.Cfg.CostExport.Name, id, func(ctx context.Context) error {
			_, err := e.C.Exports.DeleteExport(ctx, &bcmdataexports.DeleteExportInput{ExportArn: aws.String(x)})
			return err
		})
	} else {
		missing("1.1", "cost export "+e.Cfg.CostExport.Name)
	}
	bucket := e.Cfg.CostExport.Bucket
	if id, region, ok, err := BucketID(ctx, e.C, bucket); err != nil {
		return nil, err
	} else if !ok {
		missing("1.1", "cost export bucket "+bucket)
	} else if !deleteData {
		it := e.item("1.1", "cost export bucket "+bucket)
		it.Op, it.Status = plan.Info, "kept (billing history and snapshots; --delete-data removes it)"
		out = append(out, it)
	} else {
		del("1.1", "cost export bucket "+bucket, id, func(ctx context.Context) error { return EmptyAndDeleteBucket(ctx, e.C.S3In(region), bucket) })
	}
	return out, nil
}

func deletePolicy(ctx context.Context, c *iam.Client, arn string) error {
	ents, err := c.ListEntitiesForPolicy(ctx, &iam.ListEntitiesForPolicyInput{PolicyArn: aws.String(arn)})
	if err != nil {
		return err
	}
	for _, r := range ents.PolicyRoles {
		if _, err := c.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: r.RoleName, PolicyArn: aws.String(arn)}); err != nil {
			return err
		}
	}
	vs, err := c.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(arn)})
	if err != nil {
		return err
	}
	for _, v := range vs.Versions {
		if !v.IsDefaultVersion {
			if _, err := c.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{PolicyArn: aws.String(arn), VersionId: v.VersionId}); err != nil {
				return err
			}
		}
	}
	_, err = c.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: aws.String(arn)})
	return err
}

// EmptyAndDeleteBucket deletes every object (and version), then the bucket.
func EmptyAndDeleteBucket(ctx context.Context, c *s3.Client, bucket string) error {
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	for p.HasMorePages() {
		r, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		var ids []stypes.ObjectIdentifier
		for _, v := range r.Versions {
			ids = append(ids, stypes.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range r.DeleteMarkers {
			ids = append(ids, stypes.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if len(ids) == 0 {
			continue
		}
		out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &stypes.Delete{Objects: ids, Quiet: aws.Bool(true)}})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("deleting %s: %s", aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
		}
	}
	_, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	return err
}
