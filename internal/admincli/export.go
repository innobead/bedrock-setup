package admincli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bcmdataexports"
	"github.com/spf13/cobra"

	"github.com/SUSE/high-impact-ai-initiative/internal/account"
	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

func (a *App) exportCmd() *cobra.Command {
	var id, region, product string
	var models []string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write the current account, including its users, as a config file (to stdout)",
		Long: `export reads the account and prints a bedrock.yaml that matches it, so plan shows no changes.

Settings come from the file given with -f (or found as usual) when there is one; otherwise from the
account and the flags. Models can't be read from the account: pass --model when there is no file.
Only users with this id are exported.

  bedrock-admin export > bedrock.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			base := &config.Config{}
			if path, err := config.Find(a.File); err == nil {
				if base, err = config.Parse(path); err != nil {
					return err
				}
			} else if a.File != "" {
				return err
			}
			if region != "" {
				base.Region = region
			}
			c, err := awsx.New(ctx, a.profile(base), base.Region, base.Account)
			if err != nil {
				return err
			}
			cfg, err := a.export(ctx, c, base, id, product, models)
			if err != nil {
				return err
			}
			if a.JSON {
				return cli.WriteJSON(a.Out, cfg)
			}
			b, err := config.Marshal(cfg)
			if err != nil {
				return err
			}
			a.printf("# Exported from account %s by bedrock-admin export on %s.\n", c.Account, a.now().UTC().Format("2006-01-02"))
			_, err = a.Out.Write(b)
			return err
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "id to export (default: from the file, else the bedrock-deny policy's id)")
	cmd.Flags().StringVar(&region, "region", "", "Bedrock region (default: from the file, else the profile's)")
	cmd.Flags().StringVar(&product, "product", "", "product tag (default: from the file, else the users' roles)")
	cmd.Flags().StringSliceVar(&models, "model", nil, "model (inference profile ID); repeat for more (default: from the file)")
	return cmd
}

func mostCommon[T any](vals []T, key func(T) string) (T, bool) {
	counts := map[string]int{}
	var best T
	bestN, found := 0, false
	for _, v := range vals {
		k := key(v)
		counts[k]++
		if counts[k] > bestN {
			best, bestN, found = v, counts[k], true
		}
	}
	return best, found
}

func (a *App) export(ctx context.Context, c *awsx.Clients, base *config.Config, id, product string, models []string) (*config.Config, error) {
	cfg := *base
	cfg.Path = ""
	cfg.Account, cfg.Region = c.Account, c.Region
	if a.Profile != "" {
		cfg.Profile = a.Profile
	}
	if id != "" {
		cfg.ID = id
	}
	if cfg.ID == "" {
		pid, ok, err := account.PolicyID(ctx, c.IAM, policy.DenyPolicyArn(c.Account))
		if err != nil {
			return nil, err
		}
		if !ok || pid == "" {
			return nil, errors.New("no bedrock-deny policy with a bedrock-admin:id tag in this account; pass --id")
		}
		cfg.ID = pid
	}
	if len(models) > 0 {
		cfg.Models = models
	}
	if len(cfg.Models) == 0 {
		return nil, errors.New("models can't be read from the account; pass --model (repeatable) or -f with a file that sets models")
	}
	if cfg.CostExport.Name == "" {
		cfg.CostExport.Name = config.DefaultExportName
	}
	if cfg.CostExport.Bucket == "" {
		arn, err := account.FindExport(ctx, c.Exports, cfg.CostExport.Name)
		if err != nil {
			return nil, err
		}
		if arn == "" {
			return nil, fmt.Errorf("no cost export named %s; pass -f with a file that sets cost_export", cfg.CostExport.Name)
		}
		x, err := c.Exports.GetExport(ctx, &bcmdataexports.GetExportInput{ExportArn: aws.String(arn)})
		if err != nil {
			return nil, err
		}
		d := x.Export.DestinationConfigurations.S3Destination
		cfg.CostExport.Bucket, cfg.CostExport.Prefix = aws.ToString(d.S3Bucket), aws.ToString(d.S3Prefix)
	}
	if base.Path == "" {
		cfg.BlockDirectCalls = config.BlockDirectCalls{Enabled: true}
	}

	// Users: every personal role with this id.
	roles, err := account.UserRoles(ctx, c.IAM)
	if err != nil {
		return nil, err
	}
	env := a.usersEnv(ctx, &cfg, c)
	type got struct {
		r      config.Resolved
		limit  float64
		pause  int
		alerts []int
		notify string
	}
	var found []got
	for _, role := range roles {
		tags, err := env.Pause.RoleTags(ctx, role)
		if err != nil {
			return nil, err
		}
		if tags[policy.IDTag] != cfg.ID {
			continue
		}
		email := tags[policy.OwnerTag]
		if email == "" {
			continue
		}
		if product == "" && cfg.Product == "" {
			cfg.Product = tags[policy.ProductTag]
		}
		name := strings.TrimPrefix(role, "bedrock-user-")
		r := config.Resolved{Email: email, Name: name, RoleName: role, BudgetName: config.BudgetName(name)}
		o, err := env.Observe(ctx, r)
		if err != nil {
			return nil, err
		}
		g := got{r: r, limit: o.Limit, pause: o.ActionThreshold, notify: email, alerts: []int{}}
		for p := range o.Alerts {
			g.alerts = append(g.alerts, p)
		}
		sort.Ints(g.alerts)
		if len(o.ActionSubscribers) > 0 {
			g.notify = o.ActionSubscribers[0]
		}
		found = append(found, g)
	}
	if product != "" {
		cfg.Product = product
	}
	if cfg.Product == "" {
		return nil, errors.New("no product tag found; pass --product")
	}
	sort.Slice(found, func(i, j int) bool { return found[i].r.Email < found[j].r.Email })

	// Defaults: the file's when there is one, else the most common values.
	if base.Path == "" {
		if g, ok := mostCommon(found, func(g got) string { return fmt.Sprint(g.limit) }); ok {
			cfg.Defaults.LimitUSD = &g.limit
		} else {
			l := 50.0
			cfg.Defaults.LimitUSD = &l
		}
		if g, ok := mostCommon(found, func(g got) string { return fmt.Sprint(g.pause) }); ok && g.pause != config.DefaultPauseAtPercent {
			cfg.Defaults.PauseAtPercent = &g.pause
		}
		if g, ok := mostCommon(found, func(g got) string { return fmt.Sprint(g.alerts) }); ok && !slices.Equal(g.alerts, config.DefaultAlertAtPercent) {
			cfg.Defaults.AlertAtPercent = &g.alerts
		}
		cfg.Defaults.Notify = config.DefaultNotify
	}
	cfg.ApplyDefaults()
	cfg.Users = []config.User{}
	for _, g := range found {
		u := config.User{Email: g.r.Email}
		if g.r.Name != config.NameFromEmail(g.r.Email) {
			u.Name = g.r.Name
		}
		def := cfg.ResolveUser(u)
		if g.limit != def.LimitUSD {
			l := g.limit
			u.LimitUSD = &l
		}
		if g.pause != 0 && g.pause != def.PauseAtPercent {
			p := g.pause
			u.PauseAtPercent = &p
		}
		if !slices.Equal(g.alerts, def.AlertAtPercent) {
			al := g.alerts
			u.AlertAtPercent = &al
		}
		if g.notify != def.Notify {
			u.Notify = g.notify
		}
		cfg.Users = append(cfg.Users, u)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
