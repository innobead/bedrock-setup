// Package policy holds every IAM and S3 policy document the tool creates, and a way to compare
// them with what AWS returns.
package policy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
)

// Names and paths of the shared account resources.
const (
	Path                   = "/bedrock/"
	UsersPath              = "/bedrock-users/"
	DenyPolicyName         = "bedrock-deny"
	BudgetActionsRole      = "bedrock-budget-actions"
	BudgetActionsPolicy    = "attach-bedrock-deny"
	LambdaName             = "bedrock-monthly-unpause"
	LambdaRole             = "bedrock-monthly-unpause"
	LambdaPolicy           = "monthly-unpause"
	SchedulerRole          = "bedrock-monthly-unpause-scheduler"
	SchedulerPolicy        = "invoke-unpause-lambda"
	ScheduleName           = "bedrock-monthly-unpause"
	ScheduleExpression     = "cron(0 6 1 * ? *)"
	InvokePolicyName       = "bedrock-invoke"
	BlockPolicyName        = "bedrock-personal-role-only"
	BlockSid               = "BedrockOnlyViaPersonalRole"
	SSOInlinePolicyName    = "AwsSSOInlinePolicy"
	SSORolePathPrefix      = "/aws-reserved/sso.amazonaws.com/"
	OwnerTag               = "owner"
	ProductTag             = "product"
	IDTag                  = "bedrock-admin:id"
	CostAllocationOwnerTag = "iamPrincipal/owner"
)

func DenyPolicyArn(account string) string {
	return fmt.Sprintf("arn:aws:iam::%s:policy/bedrock/%s", account, DenyPolicyName)
}
func BudgetActionsRoleArn(account string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/bedrock/%s", account, BudgetActionsRole)
}
func UserRoleArn(account, role string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/bedrock-users/%s", account, role)
}
func LambdaArn(region, account string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", region, account, LambdaName)
}

// DenyActions are the model-call actions blocked by a pause.
var DenyActions = []string{"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream",
	"bedrock:CreateModelInvocationJob", "bedrock:CallWithBearerToken"}

// BlockActions are the actions Step 1.7 blocks outside personal roles.
var BlockActions = []string{"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream",
	"bedrock:CreateModelInvocationJob", "bedrock:CallWithBearerToken",
	"bedrock:InvokeAgent", "bedrock:InvokeFlow", "bedrock:RetrieveAndGenerate"}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type doc = map[string]any
type stmt = map[string]any

func document(s ...stmt) string {
	list := make([]any, len(s))
	for i := range s {
		list[i] = s[i]
	}
	return mustJSON(doc{"Version": "2012-10-17", "Statement": list})
}

// Deny is the pause policy AWS Budgets (or pause) attaches to a personal role.
func Deny() string {
	return document(stmt{"Sid": "PauseBedrockModelUsage", "Effect": "Deny", "Action": DenyActions, "Resource": "*"})
}

func BudgetActionsTrust(account string) string {
	return document(stmt{"Effect": "Allow", "Principal": doc{"Service": "budgets.amazonaws.com"},
		"Action": "sts:AssumeRole", "Condition": doc{
			"StringEquals": doc{"aws:SourceAccount": account},
			"ArnLike":      doc{"aws:SourceArn": fmt.Sprintf("arn:aws:budgets::%s:budget/*", account)}}})
}

func BudgetActionsPermissions(account string) string {
	return document(stmt{"Effect": "Allow", "Action": []string{"iam:AttachRolePolicy", "iam:DetachRolePolicy"},
		"Resource":  fmt.Sprintf("arn:aws:iam::%s:role/bedrock-users/*", account),
		"Condition": doc{"ArnEquals": doc{"iam:PolicyARN": DenyPolicyArn(account)}}})
}

func ServiceTrust(service, account string) string {
	return document(stmt{"Effect": "Allow", "Principal": doc{"Service": service}, "Action": "sts:AssumeRole",
		"Condition": doc{"StringEquals": doc{"aws:SourceAccount": account}}})
}

// LambdaPermissions lets the monthly Lambda re-arm budget actions, keep manual pauses in place and
// write snapshots.
func LambdaPermissions(region, account, bucket, snapshotPrefix string) string {
	return document(
		stmt{"Sid": "Budgets", "Effect": "Allow", "Action": []string{"budgets:DescribeBudgetActionsForAccount",
			"budgets:DescribeBudgetAction", "budgets:ExecuteBudgetAction", "budgets:DescribeBudgetActionHistories",
			"budgets:ViewBudget"}, "Resource": "*"},
		stmt{"Sid": "ReadUserRoles", "Effect": "Allow", "Action": []string{"iam:ListRoleTags", "iam:ListAttachedRolePolicies"},
			"Resource": fmt.Sprintf("arn:aws:iam::%s:role/bedrock-users/*", account)},
		stmt{"Sid": "KeepManualPauses", "Effect": "Allow", "Action": "iam:AttachRolePolicy",
			"Resource":  fmt.Sprintf("arn:aws:iam::%s:role/bedrock-users/*", account),
			"Condition": doc{"ArnEquals": doc{"iam:PolicyARN": DenyPolicyArn(account)}}},
		stmt{"Sid": "Snapshots", "Effect": "Allow", "Action": "s3:PutObject",
			"Resource": fmt.Sprintf("arn:aws:s3:::%s/%s/*", bucket, snapshotPrefix)},
		stmt{"Sid": "Logs", "Effect": "Allow", "Action": []string{"logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"},
			"Resource": fmt.Sprintf("arn:aws:logs:%s:%s:log-group:/aws/lambda/%s*", region, account, LambdaName)},
	)
}

func SchedulerPermissions(region, account string) string {
	return document(stmt{"Effect": "Allow", "Action": "lambda:InvokeFunction", "Resource": LambdaArn(region, account)})
}

// CurBucket lets only AWS Data Exports write to the cost export bucket.
func CurBucket(bucket, account string) string {
	return document(stmt{"Sid": "AllowDataExportsToWrite", "Effect": "Allow",
		"Principal": doc{"Service": []string{"bcm-data-exports.amazonaws.com", "billingreports.amazonaws.com"}},
		"Action":    []string{"s3:PutObject", "s3:GetBucketPolicy"},
		"Resource":  []string{"arn:aws:s3:::" + bucket, "arn:aws:s3:::" + bucket + "/*"},
		"Condition": doc{"StringLike": doc{"aws:SourceAccount": account,
			"aws:SourceArn": []string{fmt.Sprintf("arn:aws:cur:us-east-1:%s:definition/*", account),
				fmt.Sprintf("arn:aws:bcm-data-exports:us-east-1:%s:export/*", account)}}}})
}

// UserTrust lets only the matching SSO identity assume a personal role, with its email as the
// session name.
func UserTrust(account, email string) string {
	return document(stmt{"Effect": "Allow", "Principal": doc{"AWS": fmt.Sprintf("arn:aws:iam::%s:root", account)},
		"Action": "sts:AssumeRole", "Condition": doc{
			"ArnLike":      doc{"aws:PrincipalArn": fmt.Sprintf("arn:aws:iam::%s:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_*", account)},
			"StringLike":   doc{"aws:userid": "*:" + email},
			"StringEquals": doc{"sts:RoleSessionName": email}}})
}

// AllowedModel reports whether UserInvoke lets personal roles call a model ID or inference profile ID.
func AllowedModel(model string) bool {
	return strings.Contains(model, "anthropic.claude-") && !strings.Contains(model, "anthropic.claude-fable")
}

// UserInvoke allows Claude models only, and always denies the Claude Fable family.
func UserInvoke(account string) string {
	return document(
		stmt{"Sid": "InvokeClaudeModelsOnly", "Effect": "Allow",
			"Action": []string{"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"},
			"Resource": []string{"arn:aws:bedrock:*::foundation-model/anthropic.claude-*",
				fmt.Sprintf("arn:aws:bedrock:*:%s:inference-profile/*anthropic.claude-*", account)}},
		stmt{"Sid": "DiscoverModels", "Effect": "Allow", "Action": []string{"bedrock:ListFoundationModels",
			"bedrock:GetFoundationModel", "bedrock:ListInferenceProfiles", "bedrock:GetInferenceProfile"}, "Resource": "*"},
		stmt{"Sid": "DenyUnapprovedFableModels", "Effect": "Deny",
			"Action": []string{"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream", "bedrock:CreateModelInvocationJob"},
			"Resource": []string{"arn:aws:bedrock:*::foundation-model/anthropic.claude-fable*",
				fmt.Sprintf("arn:aws:bedrock:*:%s:inference-profile/*anthropic.claude-fable*", account)}},
	)
}

// BlockStatement is the Step 1.7 deny, as one statement (for an SCP or a permission set).
func BlockStatement(adminPermissionSet string) map[string]any {
	return stmt{"Sid": BlockSid, "Effect": "Deny", "Action": BlockActions, "Resource": "*",
		"Condition": doc{
			"ArnLike":    doc{"aws:PrincipalArn": "arn:aws:iam::*:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_*"},
			"ArnNotLike": doc{"aws:PrincipalArn": "arn:aws:iam::*:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_" + adminPermissionSet + "_*"}}}
}

// Block is the full Step 1.7 policy document, pretty-printed for the org admin.
func Block(adminPermissionSet string) string {
	b, _ := json.MarshalIndent(doc{"Version": "2012-10-17", "Statement": []any{BlockStatement(adminPermissionSet)}}, "", "  ")
	return string(b) + "\n"
}

// AssumePersonalRoles is what users' permission sets need so they can assume their personal role.
func AssumePersonalRoles() string {
	b, _ := json.MarshalIndent(doc{"Version": "2012-10-17", "Statement": []any{stmt{
		"Sid": "AssumeBedrockPersonalRole", "Effect": "Allow", "Action": "sts:AssumeRole",
		"Resource": "arn:aws:iam::*:role/bedrock-users/*"}}}, "", "  ")
	return string(b) + "\n"
}

// Decode turns a policy document as returned by IAM (URL-encoded) into plain JSON.
func Decode(s string) string {
	if strings.HasPrefix(s, "%7B") || strings.HasPrefix(s, "%7b") {
		if d, err := url.QueryUnescape(s); err == nil {
			return d
		}
	}
	return s
}

// Equal compares two policy documents, ignoring formatting, key order, the order of string lists,
// and whether a one-element list is written as a plain string.
func Equal(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(Decode(a)), &x) != nil || json.Unmarshal([]byte(Decode(b)), &y) != nil {
		return false
	}
	return reflect.DeepEqual(normalize(x), normalize(y))
}

func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalize(e)
		}
		return out
	case []any:
		if len(t) == 1 {
			return normalize(t[0])
		}
		strs := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				out := make([]any, len(t))
				for i := range t {
					out[i] = normalize(t[i])
				}
				return out
			}
			strs = append(strs, s)
		}
		sort.Strings(strs)
		out := make([]any, len(strs))
		for i, s := range strs {
			out[i] = s
		}
		return out
	}
	return v
}

// HasBlock reports whether a policy document denies every Step 1.7 action on all resources.
// The org admin may adjust the statement (for example name the account in the ARNs), so only the
// effect, actions and resource are checked.
func HasBlock(document string) bool {
	var d struct {
		Statement json.RawMessage `json:"Statement"`
	}
	if json.Unmarshal([]byte(Decode(document)), &d) != nil {
		return false
	}
	var list []map[string]any
	if json.Unmarshal(d.Statement, &list) != nil {
		var one map[string]any
		if json.Unmarshal(d.Statement, &one) != nil {
			return false
		}
		list = []map[string]any{one}
	}
	for _, s := range list {
		if s["Effect"] != "Deny" || !covers(strList(s["Resource"]), "*") {
			continue
		}
		actions := strList(s["Action"])
		ok := true
		for _, a := range BlockActions {
			if !covers(actions, a) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// AllowsAssumePersonalRole reports whether a policy document allows sts:AssumeRole on personal roles.
func AllowsAssumePersonalRole(document string) bool {
	var d struct {
		Statement json.RawMessage `json:"Statement"`
	}
	if json.Unmarshal([]byte(Decode(document)), &d) != nil {
		return false
	}
	var list []map[string]any
	if json.Unmarshal(d.Statement, &list) != nil {
		var one map[string]any
		if json.Unmarshal(d.Statement, &one) != nil {
			return false
		}
		list = []map[string]any{one}
	}
	for _, s := range list {
		if s["Effect"] != "Allow" || !covers(strList(s["Action"]), "sts:AssumeRole") {
			continue
		}
		for _, r := range strList(s["Resource"]) {
			if r == "*" || strings.Contains(r, ":role/bedrock-users/") || strings.HasSuffix(r, ":role/*") {
				return true
			}
		}
	}
	return false
}

func strList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// covers reports whether a list of IAM action or resource patterns matches want.
func covers(patterns []string, want string) bool {
	for _, p := range patterns {
		if strings.EqualFold(p, want) || p == "*" {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(strings.ToLower(want), strings.ToLower(strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}
