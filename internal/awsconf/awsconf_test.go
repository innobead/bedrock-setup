package awsconf

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = `# my config
[default]
login_session = arn:aws:iam::111122223333:user/x
region = eu-west-1

[profile sso]
sso_session = corp
sso_account_id = 111122223333
region = us-west-2

[sso-session corp]
sso_start_url = https://example.awsapps.com/start
sso_region = us-east-1

# keep me
[profile bedrock]
role_arn = old
source_profile = sso

[profile other]
s3 =
  max_concurrent_requests = 5
`

func TestParse(t *testing.T) {
	f := Parse(sample)
	if got := f.SignInProfiles(); !reflect.DeepEqual(got, []string{"default", "sso"}) {
		t.Fatalf("sign-in profiles = %v", got)
	}
	if s := f.Profile("sso-session corp"); s != nil {
		t.Fatal("sso-session is not a profile")
	}
	if f.Profile("other").Keys["max_concurrent_requests"] != "" {
		t.Fatal("nested keys are not profile keys")
	}
	if f.String() != sample {
		t.Fatal("round trip changed the file")
	}
}

func TestSetProfileReplaces(t *testing.T) {
	f := Parse(sample)
	f.SetProfile("bedrock", []KV{{"role_arn", "new"}, {"region", "us-west-2"}})
	want := strings.Replace(sample, "[profile bedrock]\nrole_arn = old\nsource_profile = sso\n",
		"[profile bedrock]\nrole_arn = new\nregion = us-west-2\n", 1)
	if f.String() != want {
		t.Fatalf("got\n%s", f.String())
	}
	if f.Profile("bedrock").Keys["role_arn"] != "new" {
		t.Fatal("not re-parsed")
	}
}

func TestSetProfileAppends(t *testing.T) {
	f := Parse("[default]\nregion = x\n")
	f.SetProfile("bedrock", []KV{{"role_arn", "a"}})
	if got := f.String(); got != "[default]\nregion = x\n\n[profile bedrock]\nrole_arn = a\n" {
		t.Fatalf("got %q", got)
	}
	e := Parse("")
	e.SetProfile("bedrock", []KV{{"role_arn", "a"}})
	if got := e.String(); got != "[profile bedrock]\nrole_arn = a\n" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteKeepsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aws", "config")
	f := Parse("[default]\n")
	if b, err := f.Write(path); err != nil || b != "" {
		t.Fatalf("first write: %q %v", b, err)
	}
	f.SetProfile("bedrock", []KV{{"region", "r"}})
	b, err := f.Write(path)
	if err != nil || b != path+".bak" {
		t.Fatalf("second write: %q %v", b, err)
	}
	old, _ := os.ReadFile(b)
	if string(old) != "[default]\n" {
		t.Fatalf("backup = %q", old)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
}
