package account

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	itypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/iamrole"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

func isNoSuchEntity(err error) bool {
	var e *itypes.NoSuchEntityException
	return errors.As(err, &e)
}

// PolicyID returns the bedrock-admin:id of a managed policy and whether it exists.
func PolicyID(ctx context.Context, c *iam.Client, arn string) (string, bool, error) {
	r, err := c.ListPolicyTags(ctx, &iam.ListPolicyTagsInput{PolicyArn: aws.String(arn)})
	if err != nil {
		if isNoSuchEntity(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading %s: %w", arn, err)
	}
	for _, t := range r.Tags {
		if aws.ToString(t.Key) == policy.IDTag {
			return aws.ToString(t.Value), true, nil
		}
	}
	return "", true, nil
}

func (e *Env) denyPolicy(ctx context.Context) ([]*plan.Item, error) {
	arn := policy.DenyPolicyArn(e.C.Account)
	it := e.item("1.2", "pause policy "+policy.DenyPolicyName)
	id, exists, err := PolicyID(ctx, e.C.IAM, arn)
	if err != nil {
		return nil, err
	}
	want := policy.Deny()
	if !exists {
		it.Op, it.Details = plan.Create, []string{"create"}
		it.Apply = func(ctx context.Context) error {
			_, err := e.C.IAM.CreatePolicy(ctx, &iam.CreatePolicyInput{PolicyName: aws.String(policy.DenyPolicyName),
				Path: aws.String(policy.Path), PolicyDocument: aws.String(want),
				Description: aws.String("Pauses Bedrock model calls; attached by AWS Budgets or bedrock-admin pause"),
				Tags:        []itypes.Tag{{Key: aws.String(policy.IDTag), Value: aws.String(e.Cfg.ID)}}})
			if err != nil {
				return fmt.Errorf("creating %s: %w", policy.DenyPolicyName, err)
			}
			return nil
		}
		return one(it), nil
	}
	if !e.owned(it, id) {
		return one(it), nil
	}
	p, err := e.C.IAM.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
	if err != nil {
		return nil, err
	}
	v, err := e.C.IAM.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn), VersionId: p.Policy.DefaultVersionId})
	if err != nil {
		return nil, err
	}
	if !policy.Equal(policy.Decode(aws.ToString(v.PolicyVersion.Document)), want) {
		it.Op, it.Details = plan.Update, []string{"policy changed outside the file"}
		it.Apply = func(ctx context.Context) error { return newPolicyVersion(ctx, e.C.IAM, arn, want) }
	}
	return one(it), nil
}

// newPolicyVersion makes doc the default version, deleting the oldest version when all 5 are used.
func newPolicyVersion(ctx context.Context, c *iam.Client, arn, doc string) error {
	vs, err := c.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(arn)})
	if err != nil {
		return err
	}
	if len(vs.Versions) >= 5 {
		old := vs.Versions
		sort.Slice(old, func(i, j int) bool { return aws.ToTime(old[i].CreateDate).Before(aws.ToTime(old[j].CreateDate)) })
		for _, v := range old {
			if !v.IsDefaultVersion {
				if _, err := c.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{PolicyArn: aws.String(arn), VersionId: v.VersionId}); err != nil {
					return err
				}
				break
			}
		}
	}
	_, err = c.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{PolicyArn: aws.String(arn),
		PolicyDocument: aws.String(doc), SetAsDefault: true})
	return err
}

// roleItem plans one account-level role.
func (e *Env) roleItem(ctx context.Context, step, label string, s iamrole.Spec) (*plan.Item, error) {
	it := e.item(step, label)
	s.Tags = e.tags()
	st, err := iamrole.Observe(ctx, e.C.IAM, s)
	if err != nil {
		return nil, err
	}
	if !st.Exists {
		it.Op, it.Details = plan.Create, []string{"create"}
		it.Apply = func(ctx context.Context) error { return iamrole.Create(ctx, e.C.IAM, s) }
		return it, nil
	}
	if !e.owned(it, st.ID()) {
		return it, nil
	}
	if len(st.Drift) > 0 {
		it.Op, it.Details = plan.Update, st.Drift
		it.Apply = func(ctx context.Context) error { return iamrole.Fix(ctx, e.C.IAM, s, st) }
	}
	return it, nil
}

// BudgetActionsSpec is the Step 1.3 role.
func BudgetActionsSpec(account string) iamrole.Spec {
	return iamrole.Spec{Name: policy.BudgetActionsRole, Path: policy.Path,
		Description: "Lets AWS Budgets attach and detach bedrock-deny on personal Bedrock roles",
		Trust:       policy.BudgetActionsTrust(account), InlineName: policy.BudgetActionsPolicy,
		Inline: policy.BudgetActionsPermissions(account)}
}

func (e *Env) budgetActionsRole(ctx context.Context) ([]*plan.Item, error) {
	it, err := e.roleItem(ctx, "1.3", "budget actions role "+policy.BudgetActionsRole, BudgetActionsSpec(e.C.Account))
	if err != nil {
		return nil, err
	}
	return one(it), nil
}
