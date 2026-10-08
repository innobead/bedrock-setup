package usercli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/SUSE/high-impact-ai-initiative/internal/cli"
)

const email = "achen@example.com"

type fakeAWS struct {
	identity   map[string]string // profile -> ARN
	assumeErr  error
	invokeErrs map[string]error
	invoked    []string
}

func (f *fakeAWS) Identity(_ context.Context, profile string) (string, error) {
	if a, ok := f.identity[profile]; ok {
		return a, nil
	}
	return "", errors.New("the SSO session has expired or is invalid")
}

func (f *fakeAWS) AssumeRole(context.Context, string, string, string) error { return f.assumeErr }

func (f *fakeAWS) Invoke(_ context.Context, _, region, model string) error {
	f.invoked = append(f.invoked, region+" "+model)
	return f.invokeErrs[model]
}

func newFake() *fakeAWS {
	return &fakeAWS{identity: map[string]string{
		"sso":     "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_Dev_abc123/" + email,
		"bedrock": "arn:aws:sts::111122223333:assumed-role/bedrock-user-achen/" + email,
	}}
}

const ssoConfig = "[profile sso]\nsso_session = corp\nregion = us-west-2\n\n[sso-session corp]\nsso_start_url = https://x\n"

func run(t *testing.T, aws *fakeAWS, config, stdin string, args ...string) (code int, out, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "config")
	if config != "" {
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AWS_CONFIG_FILE", path)
	var o, e bytes.Buffer
	a := &App{Version: "test", In: strings.NewReader(stdin), Out: &o, Err: &e, AWS: aws,
		Terminal: func() bool { return stdin != "" }}
	code = a.Main(context.Background(), args)
	return code, o.String() + e.String(), path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const wantBlock = "[profile bedrock]\nrole_arn = arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-achen\n" +
	"source_profile = sso\nrole_session_name = achen@example.com\nregion = us-west-2\n"

func TestSetupWritesProfile(t *testing.T) {
	f := newFake()
	code, out, path := run(t, f, ssoConfig, "", "setup")
	if code != cli.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := read(t, path); got != ssoConfig+"\n"+wantBlock {
		t.Fatalf("config:\n%s", got)
	}
	if read(t, path+".bak") != ssoConfig {
		t.Fatal("no backup")
	}
	for _, want := range []string{"ok    profile", "ok    SSO sign-in: achen@example.com", "ok    permission-set access",
		"ok    model us.anthropic.claude-sonnet-5-5", "All checks passed."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(f.invoked) != 3 || !strings.HasPrefix(f.invoked[0], "us-west-2 us.") {
		t.Fatalf("invoked %v", f.invoked)
	}

	// Again: nothing to change.
	code, out, _ = run(t, f, read(t, path), "", "setup")
	if code != cli.OK || !strings.Contains(out, "already set up") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestSetupFlags(t *testing.T) {
	f := newFake()
	f.identity["br"] = f.identity["bedrock"]
	code, out, path := run(t, f, ssoConfig, "", "setup", "--region", "eu-central-1",
		"--role-arn", "arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-alex", "--profile-name", "br")
	if code != cli.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	got := read(t, path)
	for _, want := range []string{"[profile br]\n", "role_arn = arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-alex\n",
		"region = eu-central-1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("config lacks %q:\n%s", want, got)
		}
	}
}

func TestSetupAsksBeforeReplacing(t *testing.T) {
	old := ssoConfig + "\n[profile bedrock]\nrole_arn = x\n"
	code, out, path := run(t, newFake(), old, "", "setup")
	if code != cli.Error || !strings.Contains(out, "--yes") || read(t, path) != old {
		t.Fatalf("no terminal: exit %d\n%s", code, out)
	}
	code, _, path = run(t, newFake(), old, "n\n", "setup")
	if code != cli.Error || read(t, path) != old {
		t.Fatalf("answered no: exit %d", code)
	}
	code, out, path = run(t, newFake(), old, "y\n", "setup")
	if code != cli.OK || !strings.HasSuffix(read(t, path), wantBlock) || read(t, path+".bak") != old {
		t.Fatalf("answered yes: exit %d\n%s", code, out)
	}
	code, _, path = run(t, newFake(), old, "", "setup", "--yes")
	if code != cli.OK || !strings.HasSuffix(read(t, path), wantBlock) {
		t.Fatalf("--yes: exit %d", code)
	}
}

func TestSetupChoosesSignInProfile(t *testing.T) {
	two := ssoConfig + "\n[profile sso2]\nsso_start_url = https://y\n"
	code, out, _ := run(t, newFake(), two, "", "setup")
	if code != cli.Error || !strings.Contains(out, "--sso-profile") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	code, out, path := run(t, newFake(), two, "nope\nsso\n", "setup")
	if code != cli.OK || !strings.Contains(read(t, path), "source_profile = sso\n") || !strings.Contains(out, "Not one of") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	code, out, _ = run(t, newFake(), "[default]\nregion = us-west-2\n", "", "setup")
	if code != cli.Error || !strings.Contains(out, "no SSO profile") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	code, out, _ = run(t, newFake(), "[profile sso]\nsso_session = corp\n", "", "setup")
	if code != cli.Error || !strings.Contains(out, "--region") {
		t.Fatalf("no region: exit %d\n%s", code, out)
	}
}

func TestSetupNotSignedIn(t *testing.T) {
	f := newFake()
	delete(f.identity, "sso")
	code, out, _ := run(t, f, ssoConfig, "", "setup")
	if code != cli.Error || !strings.Contains(out, "aws sso login --profile sso") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	f = newFake()
	f.identity["sso"] = "arn:aws:iam::111122223333:user/alex"
	if code, out, _ = run(t, f, ssoConfig, "", "setup"); code != cli.Error || !strings.Contains(out, "not with SSO") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestDoctorFailures(t *testing.T) {
	setUp := ssoConfig + "\n" + wantBlock
	cases := []struct {
		name, config string
		change       func(*fakeAWS)
		want         []string
	}{
		{"no profile", ssoConfig, nil, []string{"FAIL  profile: no profile bedrock", "fix: bedrock setup"}},
		{"inline comment", strings.Replace(setUp, "achen@example.com\n", "achen@example.com # me\n", 1), nil,
			[]string{"comment at the end of the line"}},
		{"signed out", setUp, func(f *fakeAWS) { delete(f.identity, "sso") }, []string{"FAIL  SSO sign-in", "fix: aws sso login --profile sso"}},
		{"session name", strings.Replace(setUp, "role_session_name = achen@example.com", "role_session_name = alex.chen@example.com", 1),
			nil, []string{"must be exactly your SSO email achen@example.com"}},
		{"not onboarded", setUp, func(f *fakeAWS) {
			f.assumeErr = &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized"}
		},
			[]string{"FAIL  permission-set access", "ask your admin to onboard achen@example.com"}},
		{"paused", setUp, func(f *fakeAWS) {
			f.invokeErrs = map[string]error{"us.anthropic.claude-sonnet-5-5": &smithy.GenericAPIError{
				Code: "AccessDeniedException", Message: "... with an explicit deny in an identity-based policy"}}
		}, []string{"FAIL  model us.anthropic.claude-sonnet-5-5", "paused", "ok    model us.anthropic.claude-haiku"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			if c.change != nil {
				c.change(f)
			}
			code, out, _ := run(t, f, c.config, "", "doctor")
			if code != cli.Problem {
				t.Fatalf("exit %d\n%s", code, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestDoctorJSONAndModels(t *testing.T) {
	f := newFake()
	code, out, _ := run(t, f, ssoConfig+"\n"+wantBlock, "", "doctor", "--json", "--model", "us.anthropic.claude-opus-4-1")
	if code != cli.OK || !strings.Contains(out, `"ok": true`) || len(f.invoked) != 1 || !strings.HasSuffix(f.invoked[0], "opus-4-1") {
		t.Fatalf("exit %d %v\n%s", code, f.invoked, out)
	}
}

func runClaudeCmd(t *testing.T, cfg string, runner ClaudeRunner, args ...string) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", path)
	var o bytes.Buffer
	a := &App{Version: "test", In: strings.NewReader(""), Out: &o, Err: &o, AWS: newFake(), Claude: runner}
	return a.Main(context.Background(), append([]string{"claude"}, args...)), o.String()
}

func TestClaude(t *testing.T) {
	cfg := ssoConfig + "\n" + strings.ReplaceAll(wantBlock, "us-west-2", "eu-central-1")
	t.Setenv("ANTHROPIC_API_KEY", "sk-personal")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/me/.claude-personal")
	var gotDir string
	var gotEnv, gotArgs []string
	ok := func(_ context.Context, dir string, env, args []string) (string, error) {
		gotDir, gotEnv, gotArgs = dir, env, args
		if _, err := os.Stat(dir); err != nil {
			t.Error("the temporary folder doesn't exist during the run")
		}
		return "OK\n", nil
	}
	code, out := runClaudeCmd(t, cfg, ok)
	for _, want := range []string{"ok    Claude Code with eu.anthropic.claude-sonnet-5-5: answered OK",
		`"CLAUDE_CODE_USE_BEDROCK": "1"`, `"AWS_PROFILE": "bedrock"`, `"AWS_REGION": "eu-central-1"`,
		`"ANTHROPIC_DEFAULT_HAIKU_MODEL": "eu.anthropic.claude-haiku-5-5"`, `"awsAuthRefresh": "aws sso login --profile sso"`,
		"CLAUDE_CONFIG_DIR=~/.claude-work claude", "/setup-bedrock",
		"AWS region:        eu-central-1", "AWS profile name:  bedrock",
		"Model list:        eu.anthropic.claude-sonnet-5-5  (the first is the default)\n                     eu.anthropic.claude-opus-5-5\n                     eu.anthropic.claude-haiku-5-5"} {
		if code != cli.OK || !strings.Contains(out, want) {
			t.Fatalf("exit %d, missing %q in\n%s", code, want, out)
		}
	}
	env := strings.Join(gotEnv, "\n")
	if strings.Contains(env, "sk-personal") || strings.Contains(env, ".claude-personal") ||
		!strings.Contains(env, "CLAUDE_CONFIG_DIR="+gotDir) || !strings.Contains(env, "AWS_PROFILE=bedrock") {
		t.Errorf("env:\n%s", env)
	}
	if args := strings.Join(gotArgs, " "); !strings.Contains(args, "-p Reply with OK. --model eu.anthropic.claude-sonnet-5-5 --tools  --strict-mcp-config --bare") {
		t.Errorf("args: %s", args)
	}
	if _, err := os.Stat(gotDir); !os.IsNotExist(err) {
		t.Error("the temporary folder was not removed")
	}

	denied := func(context.Context, string, []string, []string) (string, error) {
		return "API Error: AccessDeniedException: explicit deny", errors.New("exit status 1")
	}
	if code, out := runClaudeCmd(t, cfg, denied); code != cli.Problem || !strings.Contains(out, "FAIL  Claude Code") ||
		!strings.Contains(out, "fix: run bedrock doctor") || !strings.Contains(out, `"AWS_PROFILE"`) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	missing := func(context.Context, string, []string, []string) (string, error) { return "", exec.ErrNotFound }
	if code, out := runClaudeCmd(t, cfg, missing); code != cli.OK || !strings.Contains(out, "claude is not on your PATH") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	called := false
	never := func(context.Context, string, []string, []string) (string, error) { called = true; return "", nil }
	if code, out := runClaudeCmd(t, cfg, never, "--no-test"); code != cli.OK || called || strings.Contains(out, "Testing") {
		t.Fatalf("exit %d, called %v:\n%s", code, called, out)
	}
	if code, out := runClaudeCmd(t, ssoConfig, never); code != cli.Error || !strings.Contains(out, "run bedrock setup first") {
		t.Fatalf("exit %d: %s", code, out)
	}
}
