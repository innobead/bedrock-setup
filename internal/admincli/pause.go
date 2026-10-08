package admincli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/iamrole"
	"github.com/SUSE/high-impact-ai-initiative/internal/pause"
	"github.com/SUSE/high-impact-ai-initiative/internal/users"
)

// PauseJSON is the pause/unpause --json output.
type PauseJSON struct {
	Email  string        `json:"email"`
	Role   string        `json:"role"`
	Result string        `json:"result"`
	Before pause.Status  `json:"before"`
	After  *pause.Status `json:"after,omitempty"`
}

func (a *App) pauseCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "pause <email> --reason TEXT",
		Short: "Pause a user by hand (stays until unpause; the monthly Lambda leaves it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := a.clients(ctx, cfg)
			if err != nil {
				return err
			}
			env := a.usersEnv(ctx, cfg, c)
			u, err := env.Find(ctx, args[0])
			if err != nil {
				return err
			}
			st, err := iamrole.Observe(ctx, c.IAM, users.RoleSpec(cfg, c.Account, u))
			if err != nil {
				return err
			}
			if !st.Exists {
				return fmt.Errorf("%s has no role yet (run bedrock-admin apply)", u.Email)
			}
			before, err := env.Pause.Status(ctx, u.RoleName, u.BudgetName)
			if err != nil {
				return err
			}
			out := PauseJSON{Email: u.Email, Role: u.RoleName, Before: before, Result: "paused"}
			if a.DryRun {
				out.Result = "dry-run"
				a.info("Would attach bedrock-deny to %s and tag it with the reason.\n", u.RoleName)
			} else {
				if err := env.Pause.Pause(ctx, u.RoleName, awsx.Identity(c.CallerArn), reason); err != nil {
					return err
				}
				after, err := env.Pause.Status(ctx, u.RoleName, u.BudgetName)
				if err != nil {
					return err
				}
				out.After = &after
			}
			if a.JSON {
				return cli.WriteJSON(a.Out, out)
			}
			if !a.DryRun {
				a.printf("Paused %s by hand. Model calls now fail with an explicit deny.\nUndo: bedrock-admin unpause %s\n", u.Email, u.Email)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why (saved as a tag on the role)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func (a *App) unpauseCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "unpause <email> [--reason TEXT]",
		Short: "Restore a paused user's access",
		Long: `unpause restores access. A pause by hand is removed. A pause by AWS Budgets (over the limit) is
turned off until the 1st: re-arming would pause them again within hours. To restore access but keep a
cap, raise limit_usd in the file and run apply instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := a.load()
			if err != nil {
				return err
			}
			c, err := a.clients(ctx, cfg)
			if err != nil {
				return err
			}
			env := a.usersEnv(ctx, cfg, c)
			u, err := env.Find(ctx, args[0])
			if err != nil {
				return err
			}
			before, err := env.Pause.Status(ctx, u.RoleName, u.BudgetName)
			if err != nil {
				if awsx.IsNotFound(err) {
					return fmt.Errorf("%s has no role yet (run bedrock-admin apply)", u.Email)
				}
				return err
			}
			out := PauseJSON{Email: u.Email, Role: u.RoleName, Before: before}
			var res pause.Result
			switch {
			case a.DryRun:
				res = "dry-run"
			default:
				res, err = env.Pause.Unpause(ctx, u.RoleName, u.BudgetName, awsx.Identity(c.CallerArn), reason)
				if err != nil {
					return err
				}
				after, err := env.Pause.Status(ctx, u.RoleName, u.BudgetName)
				if err != nil {
					return err
				}
				out.After = &after
			}
			out.Result = string(res)
			if a.JSON {
				return cli.WriteJSON(a.Out, out)
			}
			now := a.now()
			switch res {
			case "dry-run":
				a.printf("%s is %s; unpause would change that only if paused.\n", u.Email, before.Text(now))
			case pause.NotPaused:
				a.printf("%s is not paused (%s). Nothing changed.\n", u.Email, before.Text(now))
			case pause.RemovedManual:
				a.printf("Removed the pause by hand of %s. Access restored.\n", u.Email)
			case pause.TurnedOff:
				a.printf("Unpaused %s. Their pause is off until %s, so spending is not capped this month.\n"+
					"To keep a cap instead, raise limit_usd in %s and run bedrock-admin apply.\n",
					u.Email, pause.NextMonth(now).Format("Jan 2"), cfg.Path)
			case pause.StillPausedByAWS:
				a.printf("Removed the pause by hand of %s, but AWS Budgets has also paused them (over the limit), so they\n"+
					"are still paused. Run unpause again to turn the pause off until the 1st, or raise limit_usd.\n", u.Email)
			default:
				return errors.New("unexpected result " + string(res))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why (saved as a tag on the role)")
	return cmd
}
