package admincli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
)

type configureFlags struct {
	id, account, region, product, bucket, prefix, method, adminPS string
	models                                                        []string
	limit                                                         float64
	block                                                         bool
}

func (a *App) configureCmd() *cobra.Command {
	var f configureFlags
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Write the settings part of bedrock.yaml (asks, or takes flags)",
		Long: `configure writes everything in bedrock.yaml except users:. It asks for each setting on a terminal;
with --yes, or without a terminal, it uses the flags and the defaults and asks nothing. An existing
file keeps its users: (including an !include) and any setting not given.

Writes -f, or ./bedrock.yaml. --dry-run prints the file instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.configure(cmd, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.id, "id", "", "id that tags everything apply creates")
	fl.StringVar(&f.account, "account", "", "AWS account ID (default: the profile's)")
	fl.StringVar(&f.region, "region", "", "Bedrock region (default: the profile's)")
	fl.StringVar(&f.product, "product", "", "product tag on users' roles")
	fl.StringSliceVar(&f.models, "model", nil, "model (inference profile ID); repeat for more")
	fl.Float64Var(&f.limit, "limit", 0, "default monthly limit in USD")
	fl.StringVar(&f.bucket, "bucket", "", "cost export bucket (default: bedrock-cur-<account>)")
	fl.StringVar(&f.prefix, "prefix", "", "cost export prefix (default: cur)")
	fl.BoolVar(&f.block, "block-direct-calls", true, "block model calls outside personal roles (Step 1.7)")
	fl.StringVar(&f.method, "method", "", "how the org admin applies Step 1.7: permission-set or scp")
	fl.StringVar(&f.adminPS, "admin-permission-set", "", "permission set exempt from Step 1.7 (default: BedrockAdmin)")
	return cmd
}

func (a *App) configure(cmd *cobra.Command, f configureFlags) error {
	ctx := cmd.Context()
	path := a.File
	if path == "" {
		path = "bedrock.yaml"
	}
	cur := &config.Config{}
	var doc yaml.Node
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if c, err := config.Parse(path); err == nil {
			cur = c
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if a.Profile != "" {
		cur.Profile = a.Profile
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&cur.ID, f.id)
	set(&cur.Account, f.account)
	set(&cur.Region, f.region)
	set(&cur.Product, f.product)
	set(&cur.CostExport.Bucket, f.bucket)
	set(&cur.CostExport.Prefix, f.prefix)
	set(&cur.BlockDirectCalls.Method, f.method)
	set(&cur.BlockDirectCalls.AdminPermissionSet, f.adminPS)
	if len(f.models) > 0 {
		cur.Models = f.models
	}
	if f.limit > 0 {
		cur.Defaults.LimitUSD = &f.limit
	}
	if cmd.Flags().Changed("block-direct-calls") || doc.Kind == 0 {
		cur.BlockDirectCalls.Enabled = f.block
	}
	a.fillFromAWS(ctx, cur)

	interactive := !a.Yes && cli.IsTerminal(a.In)
	if interactive {
		if err := a.ask(cur, cmd.Flags().Changed); err != nil {
			return err
		}
	}
	if cur.Region == "" {
		return errors.New("no region: the profile has none; pass --region")
	}
	if cur.ID == "" {
		cur.ID = "bedrock"
	}
	if cur.Product == "" {
		cur.Product = "bedrock"
	}
	if len(cur.Models) == 0 && cur.Region != "" {
		cur.Models = config.DefaultModels(cur.Region)
	}
	if cur.Defaults.LimitUSD == nil {
		l := 50.0
		cur.Defaults.LimitUSD = &l
	}
	cur.ApplyDefaults()
	if err := cur.Validate(); err != nil {
		return err
	}
	out, err := writeSettings(&doc, cur)
	if err != nil {
		return err
	}
	if a.DryRun {
		_, err := a.Out.Write(out)
		return err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return err
	}
	a.printf("Wrote %s. Next: add users: entries, then run bedrock-admin plan and bedrock-admin apply.\n", path)
	return nil
}

// fillFromAWS uses the profile's account and region as defaults, when the credentials work.
func (a *App) fillFromAWS(ctx context.Context, c *config.Config) {
	if c.Account != "" && c.Region != "" {
		return
	}
	cfg, err := awsx.Load(ctx, a.profile(c), c.Region)
	if err != nil {
		return
	}
	if c.Region == "" {
		c.Region = cfg.Region
	}
	if c.Account != "" {
		return
	}
	// STS needs a region; the profile may have none.
	stsCfg := cfg.Copy()
	if stsCfg.Region == "" {
		stsCfg.Region = "us-east-1"
	}
	if id, err := sts.NewFromConfig(stsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err == nil {
		c.Account = aws.ToString(id.Account)
	}
}

func (a *App) ask(c *config.Config, changed func(string) bool) error {
	p := cli.NewPrompter(a.In, a.Err)
	q := func(flag, question string, dst *string, def string) error {
		if changed(flag) {
			return nil
		}
		if *dst != "" {
			def = *dst
		}
		v, err := p.Ask(question, def)
		if err == nil {
			*dst = v
		}
		return err
	}
	if err := q("id", "id (tags everything bedrock-admin creates)", &c.ID, "bedrock"); err != nil {
		return err
	}
	if err := q("account", "AWS account ID", &c.Account, ""); err != nil {
		return err
	}
	if err := q("region", "Bedrock region", &c.Region, ""); err != nil {
		return err
	}
	if err := q("product", "product tag", &c.Product, "bedrock"); err != nil {
		return err
	}
	if !changed("model") {
		def := strings.Join(c.Models, ",")
		if def == "" && c.Region != "" {
			def = strings.Join(config.DefaultModels(c.Region), ",")
		}
		v, err := p.Ask("models (comma-separated inference profile IDs)", def)
		if err != nil {
			return err
		}
		c.Models = nil
		for _, m := range strings.Split(v, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.Models = append(c.Models, m)
			}
		}
	}
	if !changed("limit") {
		def := "50"
		if c.Defaults.LimitUSD != nil {
			def = strconv.FormatFloat(*c.Defaults.LimitUSD, 'f', -1, 64)
		}
		v, err := p.Ask("default monthly limit per user, USD", def)
		if err != nil {
			return err
		}
		l, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("limit: %w", err)
		}
		c.Defaults.LimitUSD = &l
	}
	bucketDef := ""
	if c.Account != "" {
		bucketDef = "bedrock-cur-" + c.Account
	}
	if err := q("bucket", "cost export bucket", &c.CostExport.Bucket, bucketDef); err != nil {
		return err
	}
	if !changed("block-direct-calls") {
		ok, err := p.Confirm("Block model calls outside personal roles (Step 1.7, needs your org admin)?")
		if err != nil {
			return err
		}
		c.BlockDirectCalls.Enabled = ok
	}
	if c.BlockDirectCalls.Enabled {
		if err := q("method", "how will the org admin apply it (permission-set or scp)", &c.BlockDirectCalls.Method, config.DefaultMethod); err != nil {
			return err
		}
	}
	return nil
}

// writeSettings replaces every top-level key except users: in doc with the values of c.
func writeSettings(doc *yaml.Node, c *config.Config) ([]byte, error) {
	settings := *c
	settings.Users = nil
	var fresh yaml.Node
	if err := fresh.Encode(&settings); err != nil {
		return nil, err
	}
	var users *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		m := doc.Content[0]
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == "users" {
				users = m.Content[i+1]
			}
		}
	}
	if users == nil {
		users = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	}
	// Drop users: from the encoded settings (it encodes as null) and append the kept one.
	var content []*yaml.Node
	for i := 0; i+1 < len(fresh.Content); i += 2 {
		if fresh.Content[i].Value != "users" {
			content = append(content, fresh.Content[i], fresh.Content[i+1])
		}
	}
	content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "users"}, users)
	fresh.Content = content
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{&fresh}}); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
