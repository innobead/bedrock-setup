package account

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// BlockPolicyFile is what apply writes for the org admin.
const BlockPolicyFile = "bedrock-block-policy.json"

// SSORole is one AWSReservedSSO_* role: a permission set assigned to this account.
type SSORole struct {
	PermissionSet string
	Inline        string // AwsSSOInlinePolicy, "" if none
	Managed       []string
}

// PermissionSetName extracts the permission set from AWSReservedSSO_<name>_<hash>.
func PermissionSetName(role string) string {
	n := strings.TrimPrefix(role, "AWSReservedSSO_")
	if i := strings.LastIndex(n, "_"); i > 0 {
		return n[:i]
	}
	return n
}

// BlockCheck is the result of checking every permission set.
type BlockCheck struct {
	Checked       []string
	MissingBlock  []string
	MissingAssume []string
}

// CheckSSORoles finds the permission sets (other than the admin one) missing the Step 1.7 deny or
// sts:AssumeRole on the personal roles.
func CheckSSORoles(roles []SSORole, adminPermissionSet string) BlockCheck {
	var c BlockCheck
	for _, r := range roles {
		if r.PermissionSet == adminPermissionSet {
			continue
		}
		c.Checked = append(c.Checked, r.PermissionSet)
		if !policy.HasBlock(r.Inline) {
			c.MissingBlock = append(c.MissingBlock, r.PermissionSet)
		}
		if !policy.AllowsAssumePersonalRole(r.Inline) && !broadManaged(r.Managed) {
			c.MissingAssume = append(c.MissingAssume, r.PermissionSet)
		}
	}
	sort.Strings(c.Checked)
	sort.Strings(c.MissingBlock)
	sort.Strings(c.MissingAssume)
	return c
}

func broadManaged(names []string) bool {
	for _, n := range names {
		if n == "AdministratorAccess" || n == "PowerUserAccess" {
			return true
		}
	}
	return false
}

// SSORoles reads every permission set role in the account.
func SSORoles(ctx context.Context, c *iam.Client) ([]SSORole, error) {
	var out []SSORole
	p := iam.NewListRolesPaginator(c, &iam.ListRolesInput{PathPrefix: aws.String(policy.SSORolePathPrefix)})
	for p.HasMorePages() {
		r, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, role := range r.Roles {
			name := aws.ToString(role.RoleName)
			if !strings.HasPrefix(name, "AWSReservedSSO_") {
				continue
			}
			s := SSORole{PermissionSet: PermissionSetName(name)}
			pol, err := c.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(policy.SSOInlinePolicyName)})
			switch {
			case err == nil:
				s.Inline = policy.Decode(aws.ToString(pol.PolicyDocument))
			case !isNoSuchEntity(err):
				return nil, err
			}
			ap, err := c.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(name)})
			if err != nil {
				return nil, err
			}
			for _, a := range ap.AttachedPolicies {
				s.Managed = append(s.Managed, aws.ToString(a.PolicyName))
			}
			out = append(out, s)
		}
	}
	return out, nil
}

func (e *Env) blockDirectCalls(ctx context.Context) ([]*plan.Item, error) {
	b := e.Cfg.BlockDirectCalls
	it := e.item("1.7", "block direct model calls")
	if !b.Enabled {
		it.Op, it.Status = plan.Info, "skipped (enabled: false)"
		return one(it), nil
	}
	if b.Method == "scp" {
		it.Op, it.Status = plan.Info, "applied by SCP (not checkable here)"
		return one(it), nil
	}
	roles, err := SSORoles(ctx, e.C.IAM)
	if err != nil {
		if awsx.IsAccessDenied(err) {
			it.Op, it.Status = plan.Handoff, "needs org admin (can't read the SSO roles: "+awsx.ErrorCode(err)+")"
			it.Apply = e.handoff(nil)
			return one(it), nil
		}
		return nil, fmt.Errorf("reading SSO roles: %w", err)
	}
	c := CheckSSORoles(roles, b.AdminPermissionSet)
	if len(c.Checked) == 0 {
		it.Op, it.Status = plan.Info, "no permission sets other than "+b.AdminPermissionSet+" in this account"
		return one(it), nil
	}
	if len(c.MissingAssume) > 0 {
		it.Warnings = append(it.Warnings, "permission sets without sts:AssumeRole on role/bedrock-users/* (users can't use their role): "+
			strings.Join(c.MissingAssume, ", "))
	}
	if len(c.MissingBlock) > 0 {
		it.Op, it.Status = plan.Handoff, "needs org admin: deny missing on "+strings.Join(c.MissingBlock, ", ")
		it.Apply = e.handoff(c.MissingAssume)
		return one(it), nil
	}
	it.Status = fmt.Sprintf("ok (%d permission sets carry the deny)", len(c.Checked))
	return one(it), nil
}

// handoff writes the policy file and prints the org admin instructions. It doesn't fail apply.
func (e *Env) handoff(missingAssume []string) func(context.Context) error {
	return func(context.Context) error {
		path := filepath.Join(e.Dir, BlockPolicyFile)
		if err := os.WriteFile(path, []byte(policy.Block(e.Cfg.BlockDirectCalls.AdminPermissionSet)+"\n"), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		e.logf("%s", Instructions(path, e.C.Account, e.Cfg.BlockDirectCalls.AdminPermissionSet, missingAssume))
		return nil
	}
}

// Instructions is the Step 1.7 text for the org admin.
func Instructions(path, account, adminPS string, missingAssume []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
Step 1.7 needs your AWS Organizations admin. Wrote %s. Send it with these instructions:

  Block Bedrock model calls outside personal roles in account %s, either way:
  - SCP (preferred; covers every permission set, including future ones; has no effect if %s
    is the management account): create an SCP from the file and attach it to the account.
  - Permission sets: add the statement in the file to the inline policy of every permission set
    assigned to the account except %s, then re-provision them.
`, path, account, account, adminPS)
	if len(missingAssume) > 0 {
		st, _ := json.Marshal(map[string]any{"Effect": "Allow", "Action": "sts:AssumeRole",
			"Resource": "arn:aws:iam::*:role/bedrock-users/*"})
		fmt.Fprintf(&b, `
  Also add this statement to %s, so users can use their personal role:
    %s
`, strings.Join(missingAssume, ", "), st)
	}
	fmt.Fprintf(&b, `
Then set block_direct_calls.method in bedrock.yaml to match (permission-set or scp) and run plan.
`)
	return b.String()
}
