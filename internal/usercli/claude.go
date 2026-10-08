package usercli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsconf"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
)

// ClaudeRunner runs the claude CLI in dir with env and returns its combined output.
// It returns exec.ErrNotFound when claude isn't installed.
type ClaudeRunner func(ctx context.Context, dir string, env, args []string) (string, error)

func runClaude(ctx context.Context, dir string, env, args []string) (string, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", exec.ErrNotFound
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// claudeDropEnv are variables that would make the test run use something other than the profile.
var claudeDropEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL",
	"AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_DEFAULT_PROFILE",
	"AWS_DEFAULT_REGION", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CONFIG_DIR"}

type claudeFlags struct {
	profile, model string
	noTest         bool
}

func (a *App) claudeCmd() *cobra.Command {
	var f claudeFlags
	cmd := &cobra.Command{
		Use:   "claude",
		Short: "Test Claude Code with the bedrock profile and print the settings to use",
		Long: `claude runs Claude Code once (claude -p) with the bedrock profile and the default Sonnet model, to
show that it works, then prints the Claude Code settings for the profile.

It never reads or changes your Claude Code settings: the test run uses a temporary, empty config
folder (CLAUDE_CONFIG_DIR) and working directory, no tools, MCP servers, hooks or plugins, and
removes it afterwards. You copy the printed settings yourself. The test is one short model call
through your personal role. Without claude on your PATH, or with --no-test, it only prints the
settings. It exits 2 when the test fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.claude(cmd.Context(), f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.profile, "profile-name", DefaultProfile, "profile Claude Code should use")
	fl.StringVar(&f.model, "model", "", "model for the test call (default: the region's Sonnet 5.5)")
	fl.BoolVar(&f.noTest, "no-test", false, "only print the settings")
	return cmd
}

func (a *App) claude(ctx context.Context, f claudeFlags) error {
	path, err := awsconf.Path()
	if err != nil {
		return err
	}
	file, err := awsconf.Read(path)
	if err != nil {
		return err
	}
	s := file.Profile(f.profile)
	if s == nil || s.Keys["region"] == "" {
		return fmt.Errorf("no profile %s with a region in %s: run bedrock setup first", f.profile, path)
	}
	region := s.Keys["region"]
	models := config.DefaultModels(region)
	opus, sonnet, haiku := models[0], models[1], models[2]
	if f.model == "" {
		f.model = sonnet
	}
	env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_PROFILE": f.profile, "AWS_REGION": region,
		"ANTHROPIC_MODEL": sonnet, "ANTHROPIC_DEFAULT_OPUS_MODEL": opus,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": sonnet, "ANTHROPIC_DEFAULT_HAIKU_MODEL": haiku}

	failed := false
	if !f.noTest {
		c := a.claudeTest(ctx, env, f.model)
		switch {
		case c.Name == "":
		case c.OK:
			a.printf("ok    %s: %s\n\n", c.Name, c.Detail)
		default:
			a.printf("FAIL  %s: %s\n      fix: %s\n\n", c.Name, c.Detail, c.Fix)
			failed = true
		}
	}

	settings := map[string]any{"env": env}
	if src := s.Keys["source_profile"]; src != "" {
		settings["awsAuthRefresh"] = loginCommand(file, src)
	}
	js, _ := json.MarshalIndent(settings, "  ", "  ")
	a.printf(`Claude Code settings for the %[1]s profile. bedrock doesn't write them; add them yourself:

  %[2]s

Where to put them, either way:
  - Work only: merge them into ~/.claude/settings.json. Every Claude Code session on this machine
    reads that file, so all of them then bill to your budget.
  - Next to a personal Claude account: put them in ~/.claude-work/settings.json and start Claude
    Code for work with: CLAUDE_CONFIG_DIR=~/.claude-work claude (for example as an alias in ~/.zshrc).

Or run claude, type /setup-bedrock (or /login, then 3rd-party platform, Amazon Bedrock), pick the
AWS profile %[1]s and region %[3]s: the wizard writes the same env block.

Check in Claude Code with /status: it shows Amazon Bedrock and the %[1]s profile.
%[4]s costs much more than Sonnet: switch to it with /model only when you need it.

Claude desktop app (Settings, Amazon Bedrock):
  AWS region:        %[3]s
  AWS profile name:  %[1]s
%[5]s  Model list:        %[6]s  (the first is the default)
                     %[4]s
                     %[7]s
`, f.profile, js, region, opus, awsPathLine(), sonnet, haiku)
	if failed {
		return cli.Exit(cli.Problem)
	}
	return nil
}

// claudeTest runs one claude -p call in a temporary config folder and working directory. It returns
// an empty Check when claude isn't installed.
func (a *App) claudeTest(ctx context.Context, env map[string]string, model string) cli.Check {
	name := "Claude Code with " + model
	tmp, err := os.MkdirTemp("", "bedrock-claude-")
	if err != nil {
		return cli.Check{Name: name, Detail: err.Error()}
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	var vars []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "ANTHROPIC_DEFAULT_") && !contains(claudeDropEnv, k) {
			vars = append(vars, kv)
		}
	}
	for k, v := range env {
		vars = append(vars, k+"="+v)
	}
	vars = append(vars, "ANTHROPIC_MODEL="+model, "CLAUDE_CONFIG_DIR="+tmp)

	run := a.Claude
	if run == nil {
		run = runClaude
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	a.printf("Testing Claude Code with the %s profile (one short call; your Claude Code settings are not used)...\n", env["AWS_PROFILE"])
	out, err := run(ctx, tmp, vars, []string{"-p", "Reply with OK.", "--model", model, "--tools", "",
		"--strict-mcp-config", "--bare", "--no-session-persistence"})
	switch {
	case errors.Is(err, exec.ErrNotFound):
		a.printf("claude is not on your PATH: skipped the test. Install Claude Code, then run bedrock claude again.\n\n")
		return cli.Check{}
	case err != nil:
		return cli.Check{Name: name, Detail: lastLine(out, err), Fix: claudeFix(out)}
	}
	return cli.Check{Name: name, OK: true, Detail: "answered " + strings.TrimSpace(firstLine(out))}
}

func claudeFix(out string) string {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "token") && (strings.Contains(low, "expired") || strings.Contains(low, "invalid")),
		strings.Contains(low, "sso"):
		return "your SSO sign-in expired: aws sso login (or aws login), then run bedrock claude again"
	case strings.Contains(low, "accessdenied") || strings.Contains(low, "not authorized"):
		return "run bedrock doctor: it shows whether you are paused or not onboarded"
	}
	return "run bedrock doctor; if it passes, update Claude Code (claude update) and try again"
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return truncate(l)
}

func lastLine(out string, err error) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return err.Error()
	}
	return truncate(out[strings.LastIndex(out, "\n")+1:])
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// awsPathLine is the desktop app's AWS CLI path line: it may not see the shell's PATH.
func awsPathLine() string {
	p, err := exec.LookPath("aws")
	if err != nil {
		return ""
	}
	return fmt.Sprintf("  AWS CLI path:      %s\n", p)
}
