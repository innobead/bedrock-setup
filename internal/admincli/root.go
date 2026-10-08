// Package admincli implements the bedrock-admin commands.
package admincli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/users"
)

// App holds the global flags and the streams.
type App struct {
	File    string
	Profile string
	JSON    bool
	Yes     bool
	DryRun  bool
	Quiet   bool

	In       io.Reader
	Out, Err io.Writer
	Now      func() time.Time
	Version  string
}

// New returns an App on the process's streams.
func New(version string) *App {
	return &App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Now: time.Now, Version: version}
}

// Root builds the command tree.
func (a *App) Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "bedrock-admin",
		Short: "Set up Amazon Bedrock for a team and manage per-person spending limits",
		Long: `bedrock-admin sets up an AWS account for Amazon Bedrock and manages each user's personal role,
monthly limit and pause, all from one file, bedrock.yaml.

  bedrock-admin plan      show what apply would change (exit 2 when there are changes)
  bedrock-admin apply     make the account and the users match the file
  bedrock-admin usage     where the Bedrock money went

Config file: -f, then $BEDROCK_ADMIN_CONFIG, then ./bedrock.yaml, then ~/.config/bedrock-admin/bedrock.yaml.

Exit codes: 0 ok, 1 error, 2 ran fine but found a problem (plan has changes, doctor found an issue,
usage found untracked spend).`,
		Version: a.Version,
	}
	f := root.PersistentFlags()
	f.StringVarP(&a.File, "file", "f", "", "config file (default: $BEDROCK_ADMIN_CONFIG, ./bedrock.yaml, ~/.config/bedrock-admin/bedrock.yaml)")
	f.StringVar(&a.Profile, "profile", "", "AWS profile (default: $AWS_PROFILE, then profile: in the file)")
	f.BoolVar(&a.JSON, "json", false, "print JSON")
	f.BoolVar(&a.Yes, "yes", false, "don't ask; allow removals")
	f.BoolVar(&a.DryRun, "dry-run", false, "show what would change and change nothing")
	f.BoolVar(&a.Quiet, "quiet", false, "print only changes and problems")
	root.SetIn(a.In)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	root.AddCommand(a.planCmd(), a.applyCmd(), a.exportCmd(), a.configureCmd(), a.doctorCmd(), a.uninstallCmd(),
		a.pauseCmd(), a.unpauseCmd(), a.usageCmd())
	return root
}

// Main runs the CLI and returns the exit code.
func (a *App) Main(ctx context.Context, args []string) int {
	root := a.Root()
	root.SetArgs(args)
	return cli.Run(ctx, root, a.Err)
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.Out, format, args...)
}

// info prints progress that --json and --quiet leave out.
func (a *App) info(format string, args ...any) {
	if !a.JSON && !a.Quiet {
		_, _ = fmt.Fprintf(a.Err, format, args...)
	}
}

// load finds, reads and validates the config file.
func (a *App) load() (*config.Config, error) {
	path, err := config.Find(a.File)
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// profile picks the AWS profile: --profile, then $AWS_PROFILE (left to the SDK), then the file.
func (a *App) profile(cfg *config.Config) string {
	if a.Profile != "" {
		return a.Profile
	}
	if os.Getenv("AWS_PROFILE") != "" || cfg == nil {
		return ""
	}
	return cfg.Profile
}

// clients connects to the config's account and region.
func (a *App) clients(ctx context.Context, cfg *config.Config) (*awsx.Clients, error) {
	return awsx.New(ctx, a.profile(cfg), cfg.Region, cfg.Account)
}

// profileRegion is the admin profile's own region, used to decide whether setup commands need --region.
func (a *App) profileRegion(ctx context.Context, cfg *config.Config) string {
	c, err := awsx.Load(ctx, a.profile(cfg), "")
	if err != nil {
		return ""
	}
	return c.Region
}

func (a *App) usersEnv(ctx context.Context, cfg *config.Config, c *awsx.Clients) *users.Env {
	return &users.Env{C: c, Cfg: cfg, Pause: users.NewPause(c), Out: a.Err, Now: a.Now, ProfileRegion: a.profileRegion(ctx, cfg)}
}
