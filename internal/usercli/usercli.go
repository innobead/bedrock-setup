// Package usercli is the bedrock user CLI: setup writes the personal role profile, doctor checks it.
package usercli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsconf"
	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// DefaultProfile is the profile setup writes.
const DefaultProfile = "bedrock"

// AWS is what the user CLI calls in AWS.
type AWS interface {
	// Identity returns the caller ARN of a profile.
	Identity(ctx context.Context, profile string) (string, error)
	// AssumeRole assumes roleArn with the credentials of profile.
	AssumeRole(ctx context.Context, profile, roleArn, session string) error
	// Invoke makes the smallest model call with a profile.
	Invoke(ctx context.Context, profile, region, model string) error
}

// App holds the CLI's settings and streams.
type App struct {
	Version  string
	In       io.Reader
	Out, Err io.Writer
	AWS      AWS
	// Terminal reports whether In is interactive; nil means check In.
	Terminal func() bool
	JSON     bool
	// Claude runs the claude CLI; nil means the real one.
	Claude ClaudeRunner
}

// New returns the CLI with the real AWS calls and the process streams.
func New(version string) *App {
	return &App{Version: version, In: os.Stdin, Out: os.Stdout, Err: os.Stderr, AWS: sdkAWS{}}
}

// Main runs the CLI and returns the exit code.
func (a *App) Main(ctx context.Context, args []string) int {
	root := a.Root()
	root.SetArgs(args)
	return cli.Run(ctx, root, a.Err)
}

func (a *App) printf(format string, args ...any) { fmt.Fprintf(a.Out, format, args...) }

func (a *App) terminal() bool {
	if a.Terminal != nil {
		return a.Terminal()
	}
	return cli.IsTerminal(a.In)
}

// Root returns the root command.
func (a *App) Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "bedrock",
		Short: "Set up and check your personal Amazon Bedrock profile",
		Long: `bedrock sets up the AWS profile of your personal Bedrock role, the one that counts toward your
monthly budget, and checks that it works.

  aws sso login        (or: aws login)
  bedrock setup
  bedrock claude       (tests Claude Code, prints its settings)

Exit codes: 0 ok, 1 error, 2 a check failed.`,
		Version: a.Version,
	}
	root.SetIn(a.In)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	root.AddCommand(a.setupCmd(), a.doctorCmd(), a.claudeCmd())
	return root
}

type setupFlags struct {
	ssoProfile, roleArn, region, profile string
	yes                                  bool
}

func (a *App) setupCmd() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Write the bedrock profile in ~/.aws/config, then run doctor",
		Long: `setup writes the [profile bedrock] block in ~/.aws/config (or $AWS_CONFIG_FILE).

It reads your SSO identity to find the account and your SSO email, derives your personal role
(arn:aws:iam::<account>:role/bedrock-users/bedrock-user-<name>), sets role_session_name to your SSO
email and takes the region from your SSO profile. Your admin tells you when you need --region or
--role-arn. The previous file is kept as config.bak; an existing bedrock profile is replaced only
after asking (or with --yes).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.setup(cmd.Context(), f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.ssoProfile, "sso-profile", "", "profile you sign in with (default: the only SSO or aws login profile)")
	fl.StringVar(&f.roleArn, "role-arn", "", "personal role ARN, when your admin gave you one")
	fl.StringVar(&f.region, "region", "", "Bedrock region (default: the SSO profile's region)")
	fl.StringVar(&f.profile, "profile-name", DefaultProfile, "name of the profile to write")
	fl.BoolVarP(&f.yes, "yes", "y", false, "replace an existing profile without asking")
	return cmd
}

func (a *App) setup(ctx context.Context, f setupFlags) error {
	path, err := awsconf.Path()
	if err != nil {
		return err
	}
	file, err := awsconf.Read(path)
	if err != nil {
		return err
	}
	p := cli.NewPrompter(a.In, a.Out)

	src, err := a.signInProfile(file, path, f, p)
	if err != nil {
		return err
	}
	region := f.region
	if region == "" {
		region = file.Profile(src).Keys["region"]
	}
	if region == "" {
		if !a.terminal() {
			return fmt.Errorf("profile %s has no region: pass --region (your admin knows the Bedrock region)", src)
		}
		if region, err = p.Ask("Bedrock region", "us-west-2"); err != nil {
			return err
		}
	}

	arn, err := a.AWS.Identity(ctx, src)
	if err != nil {
		return fmt.Errorf("reading your SSO identity with profile %s: %w\nSign in first: %s", src, err, loginCommand(file, src))
	}
	account, email, err := ssoIdentity(arn)
	if err != nil {
		return err
	}
	roleArn := f.roleArn
	if roleArn == "" {
		roleArn = policy.UserRoleArn(account, config.RoleName(config.NameFromEmail(email)))
	}
	kvs := []awsconf.KV{{Key: "role_arn", Value: roleArn}, {Key: "source_profile", Value: src},
		{Key: "role_session_name", Value: email}, {Key: "region", Value: region}}

	if old := file.Profile(f.profile); old != nil {
		if same(old, kvs) {
			a.printf("Profile %s in %s is already set up.\n\n", f.profile, path)
			return a.runDoctor(ctx, f.profile, nil)
		}
		a.printf("%s already has a %s profile:\n\n  %s\n\nIt becomes:\n\n  %s\n\n", path, f.profile,
			strings.Join(file.Lines[old.Start:old.End], "\n  "), strings.Join(awsconf.Block(f.profile, kvs), "\n  "))
		if !f.yes {
			if !a.terminal() {
				return errors.New("the profile exists: run again with --yes to replace it")
			}
			ok, err := p.Confirm("Replace it?")
			if err != nil {
				return err
			}
			if !ok {
				return cli.Exit(cli.Error)
			}
		}
	}
	file.SetProfile(f.profile, kvs)
	backup, err := file.Write(path)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	a.printf("Wrote profile %s to %s", f.profile, path)
	if backup != "" {
		a.printf(" (previous file: %s)", backup)
	}
	a.printf(":\n\n  %s\n\n", strings.Join(awsconf.Block(f.profile, kvs), "\n  "))
	return a.runDoctor(ctx, f.profile, nil)
}

func (a *App) signInProfile(file *awsconf.File, path string, f setupFlags, p *cli.Prompter) (string, error) {
	if f.ssoProfile != "" {
		if file.Profile(f.ssoProfile) == nil {
			return "", fmt.Errorf("no profile %s in %s", f.ssoProfile, path)
		}
		return f.ssoProfile, nil
	}
	cands := slices.DeleteFunc(file.SignInProfiles(), func(s string) bool { return s == f.profile })
	switch {
	case len(cands) == 1:
		return cands[0], nil
	case len(cands) == 0:
		return "", fmt.Errorf("no SSO profile in %s: run aws configure sso (or aws login) first, or pass --sso-profile", path)
	case !a.terminal():
		return "", fmt.Errorf("several SSO profiles in %s (%s): pass --sso-profile", path, strings.Join(cands, ", "))
	}
	for {
		ans, err := p.Ask("Profile you sign in with ("+strings.Join(cands, ", ")+")", cands[0])
		if err != nil {
			return "", err
		}
		if slices.Contains(cands, ans) {
			return ans, nil
		}
		a.printf("Not one of: %s\n", strings.Join(cands, ", "))
	}
}

func same(s *awsconf.Section, kvs []awsconf.KV) bool {
	if len(s.Keys) != len(kvs) {
		return false
	}
	for _, kv := range kvs {
		if s.Keys[kv.Key] != kv.Value {
			return false
		}
	}
	return true
}

// ssoIdentity returns the account and the session name (the SSO email) of an assumed-role ARN.
func ssoIdentity(arn string) (account, email string, err error) {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 || !strings.HasPrefix(parts[5], "assumed-role/") {
		return "", "", fmt.Errorf("signed in as %s, not with SSO: sign in with aws sso login (or aws login)", arn)
	}
	email = awsx.Identity(arn)
	if !config.IsEmail(email) {
		return "", "", fmt.Errorf("signed in as %s: the session name is not an email, so this is not an SSO login", arn)
	}
	return parts[4], email, nil
}

func loginCommand(file *awsconf.File, profile string) string {
	s := file.Profile(profile)
	if s != nil && s.Keys["login_session"] != "" {
		if profile == "default" {
			return "aws login"
		}
		return "aws login --profile " + profile
	}
	return "aws sso login --profile " + profile
}
