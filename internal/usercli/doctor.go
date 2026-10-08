package usercli

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	rtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsconf"
	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

func (a *App) doctorCmd() *cobra.Command {
	var profile string
	var models []string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check your SSO sign-in, the bedrock profile, your personal role and a model call",
		Long: `doctor checks, in order: the bedrock profile in ~/.aws/config, your SSO sign-in, that your
SSO login may use your personal role, the personal role itself and a test call to each model
(by default Claude Opus, Sonnet and Haiku 5.5: the us. or eu. inference profiles of the profile's
region, global. elsewhere).
It prints the fix for each failed check and exits 2 when one failed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runDoctor(cmd.Context(), profile, models) },
	}
	cmd.Flags().StringVar(&profile, "profile-name", DefaultProfile, "profile to check")
	cmd.Flags().StringArrayVar(&models, "model", nil, "model to test (repeatable)")
	cmd.Flags().BoolVar(&a.JSON, "json", false, "print the checks as JSON")
	return cmd
}

func (a *App) runDoctor(ctx context.Context, profile string, models []string) error {
	return cli.PrintChecks(a.Out, a.doctor(ctx, profile, models), a.JSON, false)
}

func (a *App) doctor(ctx context.Context, profile string, models []string) []cli.Check {
	var out []cli.Check
	path, err := awsconf.Path()
	if err != nil {
		return append(out, cli.Check{Name: "profile", Detail: err.Error()})
	}
	file, err := awsconf.Read(path)
	if err != nil {
		return append(out, cli.Check{Name: "profile", Detail: err.Error()})
	}
	s := file.Profile(profile)
	if s == nil {
		return append(out, cli.Check{Name: "profile", Detail: fmt.Sprintf("no profile %s in %s", profile, path),
			Fix: "bedrock setup"})
	}
	var missing []string
	for _, k := range []string{"role_arn", "source_profile", "role_session_name", "region"} {
		if s.Keys[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return append(out, cli.Check{Name: "profile", Detail: "missing " + strings.Join(missing, ", "), Fix: "bedrock setup --yes"})
	}
	for k, v := range s.Keys {
		if strings.Contains(v, " #") || strings.Contains(v, " ;") {
			return append(out, cli.Check{Name: "profile", Detail: k + " has a comment at the end of the line",
				Fix: "put comments on their own line, or run bedrock setup --yes"})
		}
	}
	roleArn, src, session, region := s.Keys["role_arn"], s.Keys["source_profile"], s.Keys["role_session_name"], s.Keys["region"]
	out = append(out, cli.Check{Name: "profile", OK: true, Detail: fmt.Sprintf("%s in %s", profile, path)})

	arn, err := a.AWS.Identity(ctx, src)
	if err != nil {
		return append(out, cli.Check{Name: "SSO sign-in", Detail: fmt.Sprintf("profile %s: %v", src, short(err)),
			Fix: loginCommand(file, src)})
	}
	_, email, err := ssoIdentity(arn)
	if err != nil {
		return append(out, cli.Check{Name: "SSO sign-in", Detail: err.Error(), Fix: "set source_profile to your SSO profile, or run bedrock setup --sso-profile <profile>"})
	}
	out = append(out, cli.Check{Name: "SSO sign-in", OK: true, Detail: email + " (profile " + src + ")"})

	if session != email {
		return append(out, cli.Check{Name: "session name",
			Detail: fmt.Sprintf("role_session_name is %s, but it must be exactly your SSO email %s", session, email),
			Fix:    "bedrock setup --yes"})
	}
	if err := a.AWS.AssumeRole(ctx, src, roleArn, session); err != nil {
		c := cli.Check{Name: "permission-set access", Detail: fmt.Sprintf("cannot use %s: %v", roleArn, short(err))}
		if awsx.IsAccessDenied(err) {
			c.Fix = "ask your admin to onboard " + email + " (and check role_arn, if they gave you one), " +
				"or to let your SSO permission set use the bedrock-users roles"
		}
		return append(out, c)
	}
	out = append(out, cli.Check{Name: "permission-set access", OK: true, Detail: "may use " + roleArn})

	arn, err = a.AWS.Identity(ctx, profile)
	if err != nil {
		return append(out, cli.Check{Name: "personal role", Detail: short(err).Error(), Fix: "bedrock setup --yes"})
	}
	out = append(out, cli.Check{Name: "personal role", OK: true, Detail: arn})

	if len(models) == 0 {
		models = config.DefaultModels(region)
	}
	for _, m := range models {
		c := cli.Check{Name: "model " + m, OK: true, Detail: "test call works"}
		if err := a.AWS.Invoke(ctx, profile, region, m); err != nil {
			c.OK, c.Detail, c.Fix = false, short(err).Error(), modelFix(err, region, m)
		}
		out = append(out, c)
	}
	return out
}

// modelFix explains a failed test call. Bedrock's AccessDenied message is often empty, so the model
// decides: the role allows every Claude model except Fable, so a denied one means the user is paused.
func modelFix(err error, region, model string) string {
	switch {
	case awsx.IsAccessDenied(err) && policy.AllowedModel(model):
		return "your Bedrock access is paused (monthly budget used up, or paused by your admin): ask your admin, or wait for the 1st"
	case awsx.IsAccessDenied(err):
		return "your role may only call Anthropic Claude models (not Fable): ask your admin if you need this one"
	case awsx.ErrorCode(err) == "ValidationException" || awsx.IsNotFound(err):
		return "this model is not offered in " + region + ": use another model, or ask your admin for the right region"
	}
	return ""
}

// short drops the SDK's operation and request ID wrapping from an error.
func short(err error) error {
	msg := err.Error()
	if i := strings.LastIndex(msg, "api error "); i >= 0 {
		return fmt.Errorf("%s", msg[i+len("api error "):])
	}
	// Without an error message the SDK prints "https response error StatusCode: 403, RequestID: <id>, <Code>: ".
	if i := strings.LastIndex(msg, "RequestID: "); i >= 0 {
		if j := strings.Index(msg[i:], ", "); j >= 0 {
			return fmt.Errorf("%s", strings.TrimRight(msg[i+j+2:], ": "))
		}
	}
	return err
}

type sdkAWS struct{}

func load(ctx context.Context, profile, region string) (aws.Config, error) {
	cfg, err := awsx.Load(ctx, profile, region)
	if err == nil && cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return cfg, err
}

func (sdkAWS) Identity(ctx context.Context, profile string) (string, error) {
	cfg, err := load(ctx, profile, "")
	if err != nil {
		return "", err
	}
	r, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(r.Arn), nil
}

func (sdkAWS) AssumeRole(ctx context.Context, profile, roleArn, session string) error {
	cfg, err := load(ctx, profile, "")
	if err != nil {
		return err
	}
	_, err = sts.NewFromConfig(cfg).AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String(roleArn),
		RoleSessionName: aws.String(session), DurationSeconds: aws.Int32(900)})
	return err
}

func (sdkAWS) Invoke(ctx context.Context, profile, region, model string) error {
	cfg, err := load(ctx, profile, region)
	if err != nil {
		return err
	}
	_, err = bedrockruntime.NewFromConfig(cfg).Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model),
		Messages: []rtypes.Message{{Role: rtypes.ConversationRoleUser,
			Content: []rtypes.ContentBlock{&rtypes.ContentBlockMemberText{Value: "Reply with OK."}}}},
		InferenceConfig: &rtypes.InferenceConfiguration{MaxTokens: aws.Int32(1)}})
	return err
}
