package admincli

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SUSE/high-impact-ai-initiative/internal/account"
	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
)

// buildPlan observes the account and the users.
func (a *App) buildPlan(ctx context.Context, cfg *config.Config, c *awsx.Clients, noDelete bool) (*plan.Plan, error) {
	p := &plan.Plan{Header: fmt.Sprintf("%s: id %s, account %s, %s, %d users", cfg.Path, cfg.ID, c.Account, cfg.Region, len(cfg.Users))}
	a.info("Reading the account setup...\n")
	acct := &account.Env{C: c, Cfg: cfg, Out: a.Err, Dir: "."}
	items, err := acct.Plan(ctx)
	if err != nil {
		return nil, err
	}
	p.Add(items...)
	a.info("Reading %d users...\n", len(cfg.Users))
	uitems, err := a.usersEnv(ctx, cfg, c).Plan(ctx, noDelete)
	if err != nil {
		return nil, err
	}
	p.Add(uitems...)
	return p, nil
}

func (a *App) showPlan(p *plan.Plan) error {
	if a.JSON {
		return plan.RenderJSON(a.Out, p)
	}
	plan.Render(a.Out, p, a.Quiet)
	return nil
}

func (a *App) planCmd() *cobra.Command {
	var noDelete bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what apply would change (read-only; exit 2 when there are changes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := a.clients(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			p, err := a.buildPlan(cmd.Context(), cfg, c, noDelete)
			if err != nil {
				return err
			}
			if err := a.showPlan(p); err != nil {
				return err
			}
			if p.Changes() > 0 {
				return cli.Exit(cli.Problem)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noDelete, "no-delete", false, "show removals as skipped")
	return cmd
}

// ApplyResult is one applied item in apply --json.
type ApplyResult struct {
	Section string   `json:"section"`
	Step    string   `json:"step,omitempty"`
	Target  string   `json:"target"`
	Op      plan.Op  `json:"op"`
	Details []string `json:"details,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// SetupCommand is a command to send a newly onboarded user.
type SetupCommand struct {
	Email   string `json:"email"`
	Command string `json:"command"`
}

// ApplyJSON is the apply --json output.
type ApplyJSON struct {
	Applied       []ApplyResult  `json:"applied"`
	Pending       []*plan.Item   `json:"pending"`
	SetupCommands []SetupCommand `json:"setup_commands"`
	Summary       string         `json:"summary"`
}

func (a *App) applyCmd() *cobra.Command {
	var noDelete bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Make the account and the users match the file",
		Long: `apply creates what's missing and fixes what differs from the file. Removals (offboarded users)
and changes that recreate a budget action need --yes; --no-delete skips removals.

Exit 2 when something is still pending afterwards (for example Step 1.7, which needs the org admin).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := a.clients(ctx, cfg)
			if err != nil {
				return err
			}
			p, err := a.buildPlan(ctx, cfg, c, noDelete)
			if err != nil {
				return err
			}
			if a.DryRun {
				if err := a.showPlan(p); err != nil {
					return err
				}
				if p.Changes() > 0 {
					return cli.Exit(cli.Problem)
				}
				return nil
			}
			var needYes []string
			for _, it := range p.Items {
				if it.NeedsYes && it.Apply != nil {
					needYes = append(needYes, it.Target)
				}
			}
			if len(needYes) > 0 && !a.Yes {
				if !a.JSON {
					plan.Render(a.Out, p, true)
				}
				return &cli.ExitError{Code: cli.Error, Err: fmt.Errorf("these changes need --yes: %s (removals can be skipped with --no-delete); nothing was changed",
					strings.Join(needYes, ", "))}
			}
			return a.apply(ctx, p)
		},
	}
	cmd.Flags().BoolVar(&noDelete, "no-delete", false, "skip removals (offboarded users)")
	return cmd
}

// awsNoise matches the SDK's request details in an error, such as
// "operation error S3: CreateBucket, https response error StatusCode: 409, RequestID: X, HostID: Y, api error ".
var awsNoise = regexp.MustCompile(`operation error [^,]+, https response error StatusCode: \d+, RequestID: [^,]*,( HostID: [^,]*,)? (api error )?`)

// shortAWSError drops the request details from an AWS SDK error; --json keeps the full text.
func shortAWSError(s string) string { return awsNoise.ReplaceAllString(s, "") }

func itemLabel(it *plan.Item) string {
	if it.Step != "" {
		return "Step " + it.Step + " " + it.Target
	}
	return it.Target
}

func (a *App) apply(ctx context.Context, p *plan.Plan) error {
	out := ApplyJSON{Applied: []ApplyResult{}, Pending: []*plan.Item{}, SetupCommands: []SetupCommand{}}
	type failure struct{ label, err string }
	var failed []failure
	counts := map[plan.Op]int{}
	accountFailed := false
	for _, it := range p.Items {
		if it.Apply == nil || !it.Op.Pending() {
			if it.Op.Pending() {
				out.Pending = append(out.Pending, it)
			}
			continue
		}
		if accountFailed && it.Section == "users" {
			it.Status = "not applied: the account setup failed; run apply again"
			out.Pending = append(out.Pending, it)
			continue
		}
		label := itemLabel(it)
		if !a.JSON {
			// A change, so --quiet prints it too.
			_, _ = fmt.Fprintf(a.Err, "%s %s: %s\n", symbol(it.Op), label, it.Summary())
		}
		r := ApplyResult{Section: it.Section, Step: it.Step, Target: it.Target, Op: it.Op, Details: it.Details}
		if err := it.Apply(ctx); err != nil {
			r.Error = err.Error()
			failed = append(failed, failure{label, shortAWSError(err.Error())})
			if !a.JSON {
				_, _ = fmt.Fprintf(a.Err, "    failed: %s\n", shortAWSError(err.Error()))
			}
			if it.Section == "account" {
				accountFailed = true
			}
		} else if it.Op == plan.Handoff {
			out.Pending = append(out.Pending, it)
		} else {
			counts[it.Op]++
			if it.Section == "users" && it.Op == plan.Create && it.SetupCommand != "" {
				out.SetupCommands = append(out.SetupCommands, SetupCommand{Email: it.Email, Command: it.SetupCommand})
			}
		}
		out.Applied = append(out.Applied, r)
	}
	var parts []string
	for _, c := range []struct {
		op   plan.Op
		text string
	}{{plan.Create, "created"}, {plan.Update, "changed"}, {plan.Delete, "removed"}} {
		if n := counts[c.op]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, c.text))
		}
	}
	switch {
	case len(parts) == 0 && len(failed) > 0:
		out.Summary = "Nothing changed."
	case len(parts) == 0:
		out.Summary = "Nothing to change."
	default:
		out.Summary = "Done: " + strings.Join(parts, ", ") + "."
	}
	if len(failed) > 0 {
		out.Summary += fmt.Sprintf(" %d failed.", len(failed))
	}
	if len(out.Pending) > 0 {
		out.Summary += fmt.Sprintf(" %d still pending.", len(out.Pending))
	}
	if a.JSON {
		if err := cli.WriteJSON(a.Out, out); err != nil {
			return err
		}
	} else {
		a.printf("\n%s\n", out.Summary)
		if len(failed) > 0 {
			a.printf("\nFailed:\n")
			for _, f := range failed {
				a.printf("  %s\n    %s\n", f.label, f.err)
			}
		}
		if len(out.Pending) > 0 {
			a.printf("\nPending:\n")
			for _, it := range out.Pending {
				a.printf("  %s\n    %s\n", itemLabel(it), it.Status)
			}
		}
		if len(out.SetupCommands) > 0 {
			a.printf("\nSend each new user their setup command (after aws sso login):\n")
			for _, s := range out.SetupCommands {
				a.printf("  %s: %s\n", s.Email, s.Command)
			}
		}
	}
	if len(failed) > 0 {
		if a.JSON {
			return cli.Exit(cli.Error)
		}
		_, _ = fmt.Fprintln(a.Err)
		return &cli.ExitError{Code: cli.Error, Err: fmt.Errorf("%d failed; run apply again after fixing the cause", len(failed))}
	}
	if len(out.Pending) > 0 {
		return cli.Exit(cli.Problem)
	}
	return nil
}

func symbol(op plan.Op) string {
	switch op {
	case plan.Create:
		return "+"
	case plan.Update:
		return "~"
	case plan.Delete:
		return "-"
	}
	return "?"
}
