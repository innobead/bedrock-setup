package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const base = `id: acme-prod
account: "123456789012"
region: us-west-2
product: genai-tools
models: [us.anthropic.claude-sonnet-4-5-20250929-v1:0]
defaults:
  limit_usd: 50
cost_export:
  bucket: acme-bedrock-cur
`

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndResolve(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "bedrock.yaml", base+`users:
  - email: Alice.Smith@acme.com
  - email: bob@acme.com
    limit_usd: 100
    pause_at_percent: 110
    alert_at_percent: []
  - email: carol@acme.com
    name: carol-x
    notify: lead@acme.com
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExport.Prefix != "cur" || c.CostExport.Name != "bedrock-cur" || c.BlockDirectCalls.AdminPermissionSet != "BedrockAdmin" {
		t.Fatalf("defaults not applied: %+v %+v", c.CostExport, c.BlockDirectCalls)
	}
	us := c.Resolve()
	a, b, cc := us[0], us[1], us[2]
	if a.Name != "alice.smith" || a.RoleName != "bedrock-user-alice.smith" || a.BudgetName != "bedrock-alice.smith" {
		t.Errorf("alice names: %+v", a)
	}
	if a.LimitUSD != 50 || a.PauseAtPercent != 100 || len(a.AlertAtPercent) != 1 || a.AlertAtPercent[0] != 80 || a.Notify != a.Email {
		t.Errorf("alice defaults: %+v", a)
	}
	if b.LimitUSD != 100 || b.PauseAtPercent != 110 || len(b.AlertAtPercent) != 0 || b.TriggerUSD() != 110 {
		t.Errorf("bob overrides: %+v trigger %v", b, b.TriggerUSD())
	}
	if cc.Name != "carol-x" || !cc.CustomName || cc.Notify != "lead@acme.com" {
		t.Errorf("carol: %+v", cc)
	}
	if c.ExportPrefix() != "cur/bedrock-cur" {
		t.Errorf("export prefix %q", c.ExportPrefix())
	}
}

func TestInclude(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "users.yaml", "- email: a@x.io\n- email: b@x.io\n")
	p := write(t, dir, "bedrock.yaml", base+"users: !include users.yaml\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Users) != 2 || c.Users[1].Email != "b@x.io" {
		t.Fatalf("include: %+v", c.Users)
	}
}

func TestIncludeLoop(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "x: !include b.yaml\n")
	write(t, dir, "b.yaml", "y: !include a.yaml\n")
	if _, err := Parse(filepath.Join(dir, "a.yaml")); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("want loop error, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"bad email": {base + "users: [{email: nope}]\n", "not an email"},
		"bad limit": {base + "users: [{email: a@x.io, limit_usd: -1}]\n", "limit_usd"},
		"limit max": {base + "users: [{email: a@x.io, limit_usd: 1000001}]\n", "at most 1000000 (got 1000001)"},
		"long name": {base + "users: [{email: a@x.io, name: " + strings.Repeat("x", 42) + "}]\n", "must be 1-41"},
		"default pause": {strings.Replace(base, "  limit_usd: 50\n", "  limit_usd: 50\n  pause_at_percent: 250\n", 1),
			"defaults.pause_at_percent: must be 1-200 (got 250)"},
		"decimals":     {base + "users: [{email: a@x.io, limit_usd: 1.234}]\n", "two decimals"},
		"bad name":     {base + "users: [{email: a@x.io, name: Bad_Name}]\n", "name:"},
		"dup email":    {base + "users: [{email: a@x.io}, {email: A@x.io}]\n", "duplicate"},
		"dup name":     {base + "users: [{email: a@x.io}, {email: a@y.io}]\n", "also used"},
		"no models":    {strings.Replace(base, "models: [us.anthropic.claude-sonnet-4-5-20250929-v1:0]", "models: []", 1), "models:"},
		"non-claude":   {strings.Replace(base, "models: [us.anthropic.claude-sonnet-4-5-20250929-v1:0]", "models: [us.meta.llama3-3-70b-instruct-v1:0]", 1), "can't be called from personal roles"},
		"fable":        {strings.Replace(base, "models: [us.anthropic.claude-sonnet-4-5-20250929-v1:0]", "models: [us.anthropic.claude-fable-1-v1:0]", 1), "can't be called from personal roles"},
		"pause 0":      {base + "users: [{email: a@x.io, pause_at_percent: 0}]\n", "1-200"},
		"pause 201":    {base + "users: [{email: a@x.io, pause_at_percent: 201}]\n", "1-200"},
		"alert 300":    {base + "users: [{email: a@x.io, alert_at_percent: [300]}]\n", "1-200"},
		"bad notify":   {base + "users: [{email: a@x.io, notify: someone}]\n", "notify"},
		"bad account":  {strings.Replace(base, `"123456789012"`, `"12"`, 1), "account"},
		"bad id":       {strings.Replace(base, "acme-prod", "Acme Prod", 1), "id:"},
		"bad method":   {base + "block_direct_calls: {method: magic}\n", "method"},
		"no limit any": {strings.Replace(base, "  limit_usd: 50\n", "", 1) + "users: [{email: a@x.io}]\n", "limit_usd: not set"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := write(t, t.TempDir(), "bedrock.yaml", tc.body)
			_, err := Load(p)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want validation error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q in %v", tc.want, err)
			}
		})
	}
}

func TestValidationListsEveryProblem(t *testing.T) {
	p := write(t, t.TempDir(), "bedrock.yaml", base+"users: [{email: nope}, {email: b@x.io, limit_usd: 0}, {email: c@x.io, pause_at_percent: 0}]\n")
	_, err := Load(p)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 3 {
		t.Fatalf("want 3 problems, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "invalid config:\n  - users[0] (nope): email: not an email address") {
		t.Fatalf("got %q", err)
	}
}

func TestPauseBounds(t *testing.T) {
	for _, v := range []string{"1", "200"} {
		p := write(t, t.TempDir(), "bedrock.yaml", base+"users: [{email: a@x.io, pause_at_percent: "+v+"}]\n")
		if _, err := Load(p); err != nil {
			t.Errorf("pause %s: %v", v, err)
		}
	}
}

func TestFindOrder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv(EnvConfig, "")
	if _, err := Find(""); err == nil {
		t.Fatal("want error when nothing exists")
	}
	write(t, dir, "bedrock.yaml", base)
	if p, _ := Find(""); p != "bedrock.yaml" {
		t.Errorf("cwd: %q", p)
	}
	t.Setenv(EnvConfig, "/env.yaml")
	if p, _ := Find(""); p != "/env.yaml" {
		t.Errorf("env: %q", p)
	}
	if p, _ := Find("/flag.yaml"); p != "/flag.yaml" {
		t.Errorf("flag: %q", p)
	}
}

func TestNameFromEmail(t *testing.T) {
	for in, want := range map[string]string{
		"alice@acme.com":     "alice",
		"Bob.Jones@acme.com": "bob.jones",
		"x+tag@acme.com":     "x-tag",
		"_a_@acme.com":       "a",
	} {
		if got := NameFromEmail(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	p := write(t, t.TempDir(), "bedrock.yaml", base+"users: [{email: a@x.io, alert_at_percent: []}]\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p2 := write(t, t.TempDir(), "b.yaml", string(out))
	c2, err := Load(p2)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if c2.Users[0].AlertAtPercent == nil || len(*c2.Users[0].AlertAtPercent) != 0 {
		t.Fatalf("empty alert list lost:\n%s", out)
	}
}
