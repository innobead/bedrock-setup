// Package config loads and validates bedrock.yaml, the single source of truth for an account's
// Bedrock setup and its users.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// Defaults applied when the file leaves a value out.
const (
	DefaultPauseAtPercent     = 100
	DefaultNotify             = "owner"
	DefaultPrefix             = "cur"
	DefaultExportName         = "bedrock-cur"
	DefaultSnapshotPrefix     = "bedrock-admin/snapshots"
	DefaultMethod             = "permission-set"
	DefaultAdminPermissionSet = "BedrockAdmin"
	EnvConfig                 = "BEDROCK_ADMIN_CONFIG"
)

// DefaultAlertAtPercent is used when neither the user nor defaults set alert_at_percent.
var DefaultAlertAtPercent = []int{80}

type Config struct {
	ID               string           `yaml:"id" json:"id"`
	Account          string           `yaml:"account" json:"account"`
	Region           string           `yaml:"region" json:"region"`
	Profile          string           `yaml:"profile,omitempty" json:"profile,omitempty"`
	Product          string           `yaml:"product" json:"product"`
	Models           []string         `yaml:"models" json:"models"`
	Defaults         Defaults         `yaml:"defaults" json:"defaults"`
	CostExport       CostExport       `yaml:"cost_export" json:"cost_export"`
	BlockDirectCalls BlockDirectCalls `yaml:"block_direct_calls" json:"block_direct_calls"`
	Users            []User           `yaml:"users" json:"users"`

	// Path is the file the config was read from ("" when built in memory).
	Path string `yaml:"-" json:"-"`
}

type Defaults struct {
	LimitUSD       *float64 `yaml:"limit_usd,omitempty" json:"limit_usd,omitempty"`
	PauseAtPercent *int     `yaml:"pause_at_percent,omitempty" json:"pause_at_percent,omitempty"`
	AlertAtPercent *[]int   `yaml:"alert_at_percent,omitempty,flow" json:"alert_at_percent,omitempty"`
	Notify         string   `yaml:"notify,omitempty" json:"notify,omitempty"`
}

type CostExport struct {
	Bucket string `yaml:"bucket" json:"bucket"`
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	// Name of the Data Exports export; default bedrock-cur.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// SnapshotPrefix is where the monthly Lambda writes snapshots; default bedrock-admin/snapshots.
	SnapshotPrefix string `yaml:"snapshot_prefix,omitempty" json:"snapshot_prefix,omitempty"`
}

type BlockDirectCalls struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	Method             string `yaml:"method,omitempty" json:"method,omitempty"`
	AdminPermissionSet string `yaml:"admin_permission_set,omitempty" json:"admin_permission_set,omitempty"`
}

type User struct {
	Email          string   `yaml:"email" json:"email"`
	Name           string   `yaml:"name,omitempty" json:"name,omitempty"`
	LimitUSD       *float64 `yaml:"limit_usd,omitempty" json:"limit_usd,omitempty"`
	PauseAtPercent *int     `yaml:"pause_at_percent,omitempty" json:"pause_at_percent,omitempty"`
	AlertAtPercent *[]int   `yaml:"alert_at_percent,omitempty,flow" json:"alert_at_percent,omitempty"`
	Notify         string   `yaml:"notify,omitempty" json:"notify,omitempty"`
}

// Resolved is a user with every default applied and every derived name filled in.
type Resolved struct {
	Email          string  `json:"email"`
	Name           string  `json:"name"`
	CustomName     bool    `json:"-"`
	RoleName       string  `json:"role"`
	BudgetName     string  `json:"budget"`
	LimitUSD       float64 `json:"limit_usd"`
	PauseAtPercent int     `json:"pause_at_percent"`
	AlertAtPercent []int   `json:"alert_at_percent"`
	Notify         string  `json:"notify"`
}

// TriggerUSD is the spend at which the user is paused.
func (r Resolved) TriggerUSD() float64 {
	return math.Round(r.LimitUSD*float64(r.PauseAtPercent)) / 100
}

var (
	idRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	accountRe = regexp.MustCompile(`^[0-9]{12}$`)
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	emailRe   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,40}$`)
	bucketRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	productRe = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,128}$`)
	psRe      = regexp.MustCompile(`^[A-Za-z0-9+=,.@-]{1,32}$`)
	invalidCh = regexp.MustCompile(`[^a-z0-9.-]+`)
)

// IsEmail reports whether s looks like an email address.
func IsEmail(s string) bool { return emailRe.MatchString(s) }

// NameFromEmail derives a user's short name from the local part of their email.
func NameFromEmail(email string) string {
	local := strings.ToLower(strings.SplitN(email, "@", 2)[0])
	n := strings.Trim(invalidCh.ReplaceAllString(local, "-"), "-.")
	if len(n) > 41 {
		n = strings.Trim(n[:41], "-.")
	}
	return n
}

func RoleName(name string) string   { return "bedrock-user-" + name }
func BudgetName(name string) string { return "bedrock-" + name }

// ValidationError lists every problem found in a file.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid config:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Find returns the config file to use: flag, then $BEDROCK_ADMIN_CONFIG, then ./bedrock.yaml, then
// ~/.config/bedrock-admin/bedrock.yaml.
func Find(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(EnvConfig); env != "" {
		return env, nil
	}
	if _, err := os.Stat("bedrock.yaml"); err == nil {
		return "bedrock.yaml", nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".config", "bedrock-admin", "bedrock.yaml")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no config file: pass -f, set $BEDROCK_ADMIN_CONFIG, or create ./bedrock.yaml (bedrock-admin configure)")
}

// Load reads, expands !include and validates a config file.
func Load(path string) (*Config, error) {
	c, err := Parse(path)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Parse reads a config file without validating it.
func Parse(path string) (*Config, error) {
	node, err := readNode(path, map[string]bool{})
	if err != nil {
		return nil, err
	}
	var c Config
	if err := node.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	c.ApplyDefaults()
	return &c, nil
}

func readNode(path string, seen map[string]bool) (*yaml.Node, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if seen[abs] {
		return nil, fmt.Errorf("%s: !include loop", path)
	}
	seen[abs] = true
	defer delete(seen, abs)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	if err := expandIncludes(&doc, filepath.Dir(path), seen); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0], nil
	}
	return &doc, nil
}

func expandIncludes(n *yaml.Node, dir string, seen map[string]bool) error {
	for i, c := range n.Content {
		if c.Tag == "!include" {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: !include needs a file name", c.Line)
			}
			p := c.Value
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			inc, err := readNode(p, seen)
			if err != nil {
				return err
			}
			n.Content[i] = inc
			continue
		}
		if err := expandIncludes(c, dir, seen); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDefaults fills in values the file may leave out.
func (c *Config) ApplyDefaults() {
	if c.CostExport.Prefix == "" {
		c.CostExport.Prefix = DefaultPrefix
	}
	if c.CostExport.Name == "" {
		c.CostExport.Name = DefaultExportName
	}
	if c.CostExport.SnapshotPrefix == "" {
		c.CostExport.SnapshotPrefix = DefaultSnapshotPrefix
	}
	c.CostExport.Prefix = strings.Trim(c.CostExport.Prefix, "/")
	c.CostExport.SnapshotPrefix = strings.Trim(c.CostExport.SnapshotPrefix, "/")
	if c.CostExport.Bucket == "" && c.Account != "" {
		c.CostExport.Bucket = "bedrock-cur-" + c.Account
	}
	if c.BlockDirectCalls.Method == "" {
		c.BlockDirectCalls.Method = DefaultMethod
	}
	if c.BlockDirectCalls.AdminPermissionSet == "" {
		c.BlockDirectCalls.AdminPermissionSet = DefaultAdminPermissionSet
	}
	if c.Defaults.Notify == "" {
		c.Defaults.Notify = DefaultNotify
	}
}

// Validate checks the whole file and reports every problem at once.
func (c *Config) Validate() error {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }

	if !idRe.MatchString(c.ID) {
		add("id: must be 1-40 lowercase letters, digits or '-', starting with a letter or digit (got %q)", c.ID)
	}
	if !accountRe.MatchString(c.Account) {
		add("account: must be a 12-digit AWS account ID, in quotes (got %q)", c.Account)
	}
	if !regionRe.MatchString(c.Region) {
		add("region: not an AWS region (got %q)", c.Region)
	}
	if !productRe.MatchString(c.Product) {
		add("product: required; letters, digits, spaces and _.:/=+@- only (got %q)", c.Product)
	}
	if len(c.Models) == 0 {
		add("models: list at least one model (inference profile ID)")
	}
	seenModel := map[string]bool{}
	for i, m := range c.Models {
		if strings.TrimSpace(m) == "" || strings.ContainsAny(m, " \t") {
			add("models[%d]: not a model ID (%q)", i, m)
		}
		if !policy.AllowedModel(m) {
			add("models[%d]: %s can't be called from personal roles (they may call Anthropic Claude models except Fable)", i, m)
		}
		if seenModel[m] {
			add("models[%d]: %s is listed twice", i, m)
		}
		seenModel[m] = true
	}
	if c.Defaults.LimitUSD != nil {
		if msg := checkLimit(*c.Defaults.LimitUSD); msg != "" {
			add("defaults.limit_usd: %s", msg)
		}
	}
	if c.Defaults.PauseAtPercent != nil && !percentOK(*c.Defaults.PauseAtPercent) {
		add("defaults.pause_at_percent: must be 1-200 (got %d)", *c.Defaults.PauseAtPercent)
	}
	if c.Defaults.AlertAtPercent != nil {
		for _, msg := range checkAlerts(*c.Defaults.AlertAtPercent) {
			add("defaults.alert_at_percent: %s", msg)
		}
	}
	if msg := checkNotify(c.Defaults.Notify); msg != "" {
		add("defaults.notify: %s", msg)
	}
	if !bucketRe.MatchString(c.CostExport.Bucket) {
		add("cost_export.bucket: not a valid S3 bucket name (got %q)", c.CostExport.Bucket)
	}
	if strings.Contains(c.CostExport.Prefix, "//") {
		add("cost_export.prefix: must not contain '//'")
	}
	if !idRe.MatchString(c.CostExport.Name) {
		add("cost_export.name: lowercase letters, digits and '-' only (got %q)", c.CostExport.Name)
	}
	switch c.BlockDirectCalls.Method {
	case "permission-set", "scp":
	default:
		add("block_direct_calls.method: must be permission-set or scp (got %q)", c.BlockDirectCalls.Method)
	}
	if !psRe.MatchString(c.BlockDirectCalls.AdminPermissionSet) {
		add("block_direct_calls.admin_permission_set: not a permission set name (got %q)", c.BlockDirectCalls.AdminPermissionSet)
	}

	emails := map[string]int{}
	names := map[string]int{}
	for i, u := range c.Users {
		where := fmt.Sprintf("users[%d]", i)
		if u.Email != "" {
			where = fmt.Sprintf("users[%d] (%s)", i, u.Email)
		}
		if !emailRe.MatchString(u.Email) {
			add("%s: email: not an email address (got %q)", where, u.Email)
		} else {
			key := strings.ToLower(u.Email)
			if j, ok := emails[key]; ok {
				add("%s: email: duplicate of users[%d]", where, j)
			}
			emails[key] = i
		}
		name := u.Name
		if name == "" {
			name = NameFromEmail(u.Email)
		}
		if !nameRe.MatchString(name) {
			add("%s: name: must be 1-41 lowercase letters, digits, '.' or '-' (got %q); set name:", where, name)
		} else {
			if j, ok := names[name]; ok {
				add("%s: name: %q is also used by users[%d]; set a different name:", where, name, j)
			}
			names[name] = i
		}
		if u.LimitUSD != nil {
			if msg := checkLimit(*u.LimitUSD); msg != "" {
				add("%s: limit_usd: %s", where, msg)
			}
		} else if c.Defaults.LimitUSD == nil {
			add("%s: limit_usd: not set, and defaults.limit_usd is not set either", where)
		}
		if u.PauseAtPercent != nil && !percentOK(*u.PauseAtPercent) {
			add("%s: pause_at_percent: must be 1-200 (got %d)", where, *u.PauseAtPercent)
		}
		if u.AlertAtPercent != nil {
			for _, msg := range checkAlerts(*u.AlertAtPercent) {
				add("%s: alert_at_percent: %s", where, msg)
			}
		}
		if u.Notify != "" {
			if msg := checkNotify(u.Notify); msg != "" {
				add("%s: notify: %s", where, msg)
			}
		}
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

func checkLimit(v float64) string {
	if v <= 0 || v > 1_000_000 || math.IsNaN(v) {
		return fmt.Sprintf("must be more than 0 and at most 1000000 (got %s)", strconv.FormatFloat(v, 'f', -1, 64))
	}
	if math.Abs(v*100-math.Round(v*100)) > 1e-6 {
		return fmt.Sprintf("at most two decimals (got %v)", v)
	}
	return ""
}

func percentOK(v int) bool { return v >= 1 && v <= 200 }

func checkAlerts(a []int) []string {
	var p []string
	seen := map[int]bool{}
	for _, v := range a {
		if !percentOK(v) {
			p = append(p, fmt.Sprintf("each value must be 1-200 (got %d)", v))
		}
		if seen[v] {
			p = append(p, fmt.Sprintf("%d is listed twice", v))
		}
		seen[v] = true
	}
	if len(a) > 4 {
		p = append(p, "at most 4 alerts (AWS Budgets allows 5 notifications per budget; the pause uses one)")
	}
	return p
}

func checkNotify(v string) string {
	if v == "owner" || emailRe.MatchString(v) {
		return ""
	}
	return fmt.Sprintf("must be \"owner\" or an email address (got %q)", v)
}

// Resolve applies defaults to every user. Call Validate first.
func (c *Config) Resolve() []Resolved {
	out := make([]Resolved, 0, len(c.Users))
	for _, u := range c.Users {
		out = append(out, c.ResolveUser(u))
	}
	return out
}

func (c *Config) ResolveUser(u User) Resolved {
	r := Resolved{Email: u.Email, Name: u.Name, CustomName: u.Name != ""}
	if r.Name == "" {
		r.Name = NameFromEmail(u.Email)
	}
	r.RoleName, r.BudgetName = RoleName(r.Name), BudgetName(r.Name)
	switch {
	case u.LimitUSD != nil:
		r.LimitUSD = *u.LimitUSD
	case c.Defaults.LimitUSD != nil:
		r.LimitUSD = *c.Defaults.LimitUSD
	}
	r.PauseAtPercent = DefaultPauseAtPercent
	if u.PauseAtPercent != nil {
		r.PauseAtPercent = *u.PauseAtPercent
	} else if c.Defaults.PauseAtPercent != nil {
		r.PauseAtPercent = *c.Defaults.PauseAtPercent
	}
	alerts := DefaultAlertAtPercent
	if u.AlertAtPercent != nil {
		alerts = *u.AlertAtPercent
	} else if c.Defaults.AlertAtPercent != nil {
		alerts = *c.Defaults.AlertAtPercent
	}
	r.AlertAtPercent = append([]int{}, alerts...)
	sort.Ints(r.AlertAtPercent)
	notify := u.Notify
	if notify == "" {
		notify = c.Defaults.Notify
	}
	if notify == "" || notify == "owner" {
		notify = u.Email
	}
	r.Notify = notify
	return r
}

// FindUser returns the resolved user with this email (case-insensitive) or name.
func (c *Config) FindUser(who string) (Resolved, bool) {
	for _, r := range c.Resolve() {
		if strings.EqualFold(r.Email, who) || r.Name == who {
			return r, true
		}
	}
	return Resolved{}, false
}

// ExportPrefix is the S3 key prefix the export writes to: <prefix>/<name>.
func (c *Config) ExportPrefix() string {
	if c.CostExport.Prefix == "" {
		return c.CostExport.Name
	}
	return c.CostExport.Prefix + "/" + c.CostExport.Name
}

// Marshal renders a config as YAML, the way export and configure write it.
func Marshal(c *Config) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
