package account

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	ltypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schtypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/iamrole"
	"github.com/SUSE/high-impact-ai-initiative/internal/lambdacode"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// Environment variables of the monthly Lambda.
const (
	EnvSnapshotBucket = "SNAPSHOT_BUCKET"
	EnvSnapshotPrefix = "SNAPSHOT_PREFIX"
	EnvDenyPolicyArn  = "DENY_POLICY_ARN"
)

const scheduleIDPrefix = "bedrock-admin:id="

// LambdaRoleSpec and SchedulerRoleSpec are the Step 1.4 roles.
func LambdaRoleSpec(region, account, bucket, snapshotPrefix string) iamrole.Spec {
	return iamrole.Spec{Name: policy.LambdaRole, Path: policy.Path,
		Description: "Runs the monthly Bedrock unpause Lambda",
		Trust:       policy.ServiceTrust("lambda.amazonaws.com", account), InlineName: policy.LambdaPolicy,
		Inline: policy.LambdaPermissions(region, account, bucket, snapshotPrefix)}
}

func SchedulerRoleSpec(region, account string) iamrole.Spec {
	return iamrole.Spec{Name: policy.SchedulerRole, Path: policy.Path,
		Description: "Lets EventBridge Scheduler invoke the monthly Bedrock unpause Lambda",
		Trust:       policy.ServiceTrust("scheduler.amazonaws.com", account), InlineName: policy.SchedulerPolicy,
		Inline: policy.SchedulerPermissions(region, account)}
}

func (e *Env) lambdaEnv() map[string]string {
	return map[string]string{
		EnvSnapshotBucket: e.Cfg.CostExport.Bucket,
		EnvSnapshotPrefix: e.Cfg.CostExport.SnapshotPrefix,
		EnvDenyPolicyArn:  policy.DenyPolicyArn(e.C.Account),
	}
}

func (e *Env) monthlyLambda(ctx context.Context) ([]*plan.Item, error) {
	region, account := e.Cfg.Region, e.C.Account
	lr, err := e.roleItem(ctx, "1.4", "Lambda role "+policy.LambdaRole,
		LambdaRoleSpec(region, account, e.Cfg.CostExport.Bucket, e.Cfg.CostExport.SnapshotPrefix))
	if err != nil {
		return nil, err
	}
	sr, err := e.roleItem(ctx, "1.4", "scheduler role "+policy.SchedulerRole, SchedulerRoleSpec(region, account))
	if err != nil {
		return nil, err
	}
	fn, err := e.function(ctx)
	if err != nil {
		return nil, err
	}
	sch, err := e.schedule(ctx)
	if err != nil {
		return nil, err
	}
	return []*plan.Item{lr, sr, fn, sch}, nil
}

func (e *Env) function(ctx context.Context) (*plan.Item, error) {
	it := e.item("1.4", "Lambda "+policy.LambdaName)
	zip, zerr := lambdacode.Zip()
	got, err := e.C.Lambda.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: aws.String(policy.LambdaName)})
	if err != nil && !awsx.IsNotFound(err) {
		return nil, fmt.Errorf("reading Lambda %s: %w", policy.LambdaName, err)
	}
	env := e.lambdaEnv()
	if err != nil {
		if zerr != nil {
			it.Op, it.Status = plan.Conflict, zerr.Error()
			return it, nil
		}
		it.Op, it.Details = plan.Create, []string{"create (runs 06:00 UTC on the 1st)"}
		it.Apply = func(ctx context.Context) error {
			in := &lambda.CreateFunctionInput{FunctionName: aws.String(policy.LambdaName),
				Role:    aws.String(fmt.Sprintf("arn:aws:iam::%s:role%s%s", e.C.Account, policy.Path, policy.LambdaRole)),
				Runtime: ltypes.RuntimeProvidedal2023, Handler: aws.String("bootstrap"),
				Architectures: []ltypes.Architecture{ltypes.ArchitectureArm64},
				Code:          &ltypes.FunctionCode{ZipFile: zip}, Timeout: aws.Int32(300), MemorySize: aws.Int32(128),
				Description: aws.String("Unpauses and re-arms Bedrock users on the 1st; saves the monthly snapshot (bedrock-admin)"),
				Environment: &ltypes.Environment{Variables: env}, Tags: e.tags()}
			// A new role can't be assumed by Lambda for a few seconds.
			err := iamrole.Retry(ctx, 90*time.Second, func(err error) bool {
				var ip *ltypes.InvalidParameterValueException
				return errors.As(err, &ip) && strings.Contains(err.Error(), "assume")
			}, func() error { _, err := e.C.Lambda.CreateFunction(ctx, in); return err })
			if err != nil {
				return fmt.Errorf("creating Lambda %s: %w", policy.LambdaName, err)
			}
			return lambda.NewFunctionActiveV2Waiter(e.C.Lambda).Wait(ctx,
				&lambda.GetFunctionInput{FunctionName: aws.String(policy.LambdaName)}, 2*time.Minute)
		}
		return it, nil
	}
	if !e.owned(it, got.Tags[policy.IDTag]) {
		return it, nil
	}
	if zerr != nil {
		it.Op, it.Status = plan.Conflict, zerr.Error()
		return it, nil
	}
	c := got.Configuration
	codeChanged := aws.ToString(c.CodeSha256) != lambdacode.Sha256(zip)
	var have map[string]string
	if c.Environment != nil {
		have = c.Environment.Variables
	}
	envChanged := !maps.Equal(have, env)
	if codeChanged {
		it.Details = append(it.Details, "update code (deployed code differs from this bedrock-admin's)")
	}
	if envChanged {
		it.Details = append(it.Details, "settings changed outside the file")
	}
	if codeChanged || envChanged {
		it.Op = plan.Update
		it.Apply = func(ctx context.Context) error {
			w := lambda.NewFunctionUpdatedV2Waiter(e.C.Lambda)
			get := &lambda.GetFunctionInput{FunctionName: aws.String(policy.LambdaName)}
			if codeChanged {
				if _, err := e.C.Lambda.UpdateFunctionCode(ctx, &lambda.UpdateFunctionCodeInput{
					FunctionName: aws.String(policy.LambdaName), ZipFile: zip,
					Architectures: []ltypes.Architecture{ltypes.ArchitectureArm64}}); err != nil {
					return fmt.Errorf("updating Lambda code: %w", err)
				}
				if err := w.Wait(ctx, get, 2*time.Minute); err != nil {
					return err
				}
			}
			if envChanged {
				if _, err := e.C.Lambda.UpdateFunctionConfiguration(ctx, &lambda.UpdateFunctionConfigurationInput{
					FunctionName: aws.String(policy.LambdaName), Environment: &ltypes.Environment{Variables: env}}); err != nil {
					return fmt.Errorf("updating Lambda settings: %w", err)
				}
				return w.Wait(ctx, get, 2*time.Minute)
			}
			return nil
		}
	}
	return it, nil
}

// ScheduleID reads the bedrock-admin id from a schedule's description (schedules have no tags).
func ScheduleID(description string) string {
	for _, f := range strings.Fields(description) {
		if v, ok := strings.CutPrefix(strings.TrimPrefix(f, "("), scheduleIDPrefix); ok {
			return strings.TrimSuffix(v, ")")
		}
	}
	return ""
}

func (e *Env) schedule(ctx context.Context) (*plan.Item, error) {
	it := e.item("1.4", "schedule "+policy.ScheduleName)
	desc := fmt.Sprintf("Runs %s at 06:00 UTC on the 1st (%s%s)", policy.LambdaName, scheduleIDPrefix, e.Cfg.ID)
	target := &schtypes.Target{Arn: aws.String(policy.LambdaArn(e.Cfg.Region, e.C.Account)),
		RoleArn: aws.String(fmt.Sprintf("arn:aws:iam::%s:role%s%s", e.C.Account, policy.Path, policy.SchedulerRole))}
	got, err := e.C.Scheduler.GetSchedule(ctx, &scheduler.GetScheduleInput{Name: aws.String(policy.ScheduleName)})
	if err != nil && !awsx.IsNotFound(err) {
		return nil, fmt.Errorf("reading schedule %s: %w", policy.ScheduleName, err)
	}
	if err != nil {
		it.Op, it.Details = plan.Create, []string{"create (" + policy.ScheduleExpression + " UTC)"}
		it.Apply = func(ctx context.Context) error {
			in := &scheduler.CreateScheduleInput{Name: aws.String(policy.ScheduleName),
				ScheduleExpression: aws.String(policy.ScheduleExpression), ScheduleExpressionTimezone: aws.String("UTC"),
				FlexibleTimeWindow: &schtypes.FlexibleTimeWindow{Mode: schtypes.FlexibleTimeWindowModeOff},
				Target:             target, Description: aws.String(desc), State: schtypes.ScheduleStateEnabled}
			err := iamrole.Retry(ctx, 60*time.Second, func(err error) bool { return awsx.ErrorCode(err) == "ValidationException" },
				func() error { _, err := e.C.Scheduler.CreateSchedule(ctx, in); return err })
			if err != nil {
				return fmt.Errorf("creating schedule %s: %w", policy.ScheduleName, err)
			}
			return nil
		}
		return it, nil
	}
	if !e.owned(it, ScheduleID(aws.ToString(got.Description))) {
		return it, nil
	}
	var d []string
	if aws.ToString(got.ScheduleExpression) != policy.ScheduleExpression || aws.ToString(got.ScheduleExpressionTimezone) != "UTC" {
		d = append(d, fmt.Sprintf("schedule %s %s → %s UTC", aws.ToString(got.ScheduleExpression),
			aws.ToString(got.ScheduleExpressionTimezone), policy.ScheduleExpression))
	}
	if got.State != schtypes.ScheduleStateEnabled {
		d = append(d, "disabled → enabled")
	}
	if got.Target == nil || aws.ToString(got.Target.Arn) != aws.ToString(target.Arn) || aws.ToString(got.Target.RoleArn) != aws.ToString(target.RoleArn) {
		d = append(d, "target changed outside the file")
	}
	if len(d) > 0 {
		it.Op, it.Details = plan.Update, d
		it.Apply = func(ctx context.Context) error {
			_, err := e.C.Scheduler.UpdateSchedule(ctx, &scheduler.UpdateScheduleInput{Name: aws.String(policy.ScheduleName),
				ScheduleExpression: aws.String(policy.ScheduleExpression), ScheduleExpressionTimezone: aws.String("UTC"),
				FlexibleTimeWindow: &schtypes.FlexibleTimeWindow{Mode: schtypes.FlexibleTimeWindowModeOff},
				Target:             target, Description: aws.String(desc), State: schtypes.ScheduleStateEnabled})
			return err
		}
	}
	return it, nil
}
