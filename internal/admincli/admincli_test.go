package admincli

import (
	"bytes"
	"context"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	a := New("test")
	a.In, a.Out, a.Err = strings.NewReader(""), &out, &errb
	code := a.Main(context.Background(), args)
	return code, out.String(), errb.String()
}

func TestHelpAndVersion(t *testing.T) {
	code, out, _ := run(t, "--help")
	if code != 0 || !strings.Contains(out, "bedrock-admin plan") {
		t.Fatalf("help: code %d\n%s", code, out)
	}
	code, out, _ = run(t, "--version")
	if code != 0 || !strings.Contains(out, "test") {
		t.Fatalf("version: code %d %q", code, out)
	}
}

func TestBadConfigIsError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bedrock.yaml")
	if err := os.WriteFile(f, []byte("id: Bad_ID\naccount: 12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "plan", "-f", f)
	if code != 1 || !strings.Contains(errs, "account:") || !strings.Contains(errs, "id:") {
		t.Fatalf("code %d\n%s", code, errs)
	}
	code, _, _ = run(t, "plan", "-f", filepath.Join(dir, "missing.yaml"))
	if code != 1 {
		t.Fatalf("missing file: code %d", code)
	}
}

func TestUnknownCommandIsError(t *testing.T) {
	if code, _, _ := run(t, "frobnicate"); code != 1 {
		t.Fatalf("code %d", code)
	}
}

func TestConfigureKeepsUsersInclude(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bedrock.yaml")
	if err := os.WriteFile(f, []byte("id: team\nproduct: p\nusers: !include users.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.yaml"), []byte("- email: a@example.test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_PROFILE", "bedrock-admin-test-none")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none"))
	code, _, errs := run(t, "configure", "--yes", "-f", f, "--account", "123456789012", "--region", "eu-central-1", "--limit", "75")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, errs)
	}
	b, _ := os.ReadFile(f)
	s := string(b)
	for _, want := range []string{"users: !include users.yaml", "id: team", "product: p", "limit_usd: 75", "eu.anthropic.claude", `account: "123456789012"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestConfigureNewFileDefaults(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bedrock.yaml")
	t.Setenv("AWS_PROFILE", "bedrock-admin-test-none")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "none"))
	code, out, errs := run(t, "configure", "--yes", "--dry-run", "-f", f, "--account", "123456789012", "--region", "ap-northeast-1")
	if code != 0 {
		t.Fatalf("code %d\n%s", code, errs)
	}
	for _, want := range []string{"enabled: true", "global.anthropic.claude-opus-5-5", "users: []", "bucket: bedrock-cur-123456789012"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("--dry-run wrote the file")
	}
}

func TestDefaultModels(t *testing.T) {
	for region, want := range map[string]string{
		"us-west-2":      "us.anthropic.claude-opus-5-5,us.anthropic.claude-sonnet-5-5,us.anthropic.claude-haiku-5-5",
		"eu-central-1":   "eu.anthropic.claude-opus-5-5,eu.anthropic.claude-sonnet-5-5,eu.anthropic.claude-haiku-5-5",
		"ap-northeast-1": "global.anthropic.claude-opus-5-5,global.anthropic.claude-sonnet-5-5,global.anthropic.claude-haiku-5-5",
	} {
		m := config.DefaultModels(region)
		if got := strings.Join(m, ","); got != want {
			t.Errorf("%s: %s", region, got)
		}
		for _, id := range m {
			if !policy.AllowedModel(id) {
				t.Errorf("%s: personal roles can't call %s", region, id)
			}
		}
	}
}

func TestUsageMonths(t *testing.T) {
	got, err := usageMonths(usageFlags{months: 3, monthsSet: true}, "2026-02")
	if err != nil || strings.Join(got, ",") != "2025-12,2026-01,2026-02" {
		t.Fatal(got, err)
	}
	if got, _ := usageMonths(usageFlags{}, "2026-02"); strings.Join(got, ",") != "2026-02" {
		t.Fatal(got)
	}
	for _, f := range []usageFlags{{month: "2026-03"}, {month: "26-1"}, {month: "2026-01", months: 2, monthsSet: true}, {months: 25, monthsSet: true}, {months: 0, monthsSet: true}} {
		if _, err := usageMonths(f, "2026-02"); err == nil {
			t.Errorf("%+v: no error", f)
		}
	}
	if code, _, errs := run(t, "usage", "a@example.test", "--months", "2"); code != 1 || !strings.Contains(errs, "--month") {
		t.Fatalf("code %d %s", code, errs)
	}
}
