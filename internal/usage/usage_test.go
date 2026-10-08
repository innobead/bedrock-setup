package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/SUSE/high-impact-ai-initiative/internal/pause"
)

// The fixtures in tests/fixtures are shared with tests/e2e/suites/usage.robot. Regenerate them with
//
//	go test ./internal/usage -run TestFixtures -update
//
// and keep tests/fixtures/README.md (the expected numbers) in step.
var update = flag.Bool("update", false, "rewrite tests/fixtures")

const fixtures = "../../tests/fixtures"

type fixtureRow struct {
	Payer     string            `parquet:"bill_payer_account_id"`
	Principal string            `parquet:"line_item_iam_principal,optional"`
	Code      string            `parquet:"line_item_product_code"`
	UsageType string            `parquet:"line_item_usage_type"`
	Cost      float64           `parquet:"line_item_unblended_cost"`
	Start     time.Time         `parquet:"line_item_usage_start_date,timestamp(millisecond)"`
	Tags      map[string]string `parquet:"tags"`
	Product   map[string]string `parquet:"product"`
}

const acct = "111122223333"

func sts(role, session string) string {
	return "arn:aws:sts::" + acct + ":assumed-role/" + role + "/" + session
}

func iamUser(name string) string { return "arn:aws:iam::" + acct + ":user/" + name }

const (
	sonnet = "Claude Sonnet 4.5 (Amazon Bedrock Edition)"
	haiku  = "Claude Haiku 4.5 (Amazon Bedrock Edition)"
)

func row(principal, owner string, cost float64, day string, model string) fixtureRow {
	t, err := time.Parse("2006-01-02 15", day)
	if err != nil {
		panic(err)
	}
	r := fixtureRow{Payer: acct, Principal: principal, Code: "AmazonBedrock", UsageType: "USE1-MP:USE1_InputTokenCount-Units", Cost: cost, Start: t,
		Tags: map[string]string{}, Product: map[string]string{"product_name": model, "region": "us-east-1"}}
	if owner != "" {
		r.Tags[ownerTag] = owner
		r.Tags["user:product"] = "bedrock-e2e"
	}
	return r
}

// thisMonth: see tests/fixtures/README.md for the expected report.
func thisMonth() []fixtureRow {
	sso := "AWSReservedSSO_PowerUser_0123456789abcdef"
	return []fixtureRow{
		row(sts("bedrock-user-usage-a", "usage-a@example.test"), "usage-a@example.test", 10.00, "2026-10-01 09", sonnet),
		row(sts("bedrock-user-usage-a", "usage-a@example.test"), "usage-a@example.test", 2.50, "2026-10-02 10", haiku),
		row(sts("bedrock-user-usage-a", "usage-a@example.test"), "usage-a@example.test", 0.75, "2026-10-02 11", sonnet),
		row(sts("bedrock-user-usage-b", "usage-b@example.test"), "usage-b@example.test", 4.00, "2026-10-03 08", sonnet),
		row(sts("bedrock-user-usage-c", "usage-c@example.test"), "usage-c@example.test", 1.20, "2026-10-03 12", haiku),
		// Owner tag but no budget: untracked.
		row(sts("bedrock-user-usage-ghost", "usage-ghost@example.test"), "usage-ghost@example.test", 0.80, "2026-10-04 09", sonnet),
		// Personal role without the owner tag.
		row(sts("bedrock-user-usage-x", "usage-x@example.test"), "", 0.10, "2026-10-04 10", haiku),
		// SSO: also has spend in its personal role? No: a separate identity.
		row(sts(sso, "usage-sso@example.test"), "", 2.30, "2026-10-02 14", sonnet),
		row(sts(sso, "usage-sso@example.test"), "", 0.40, "2026-10-05 15", haiku),
		// usage-a calling through SSO, outside the personal role.
		row(sts(sso, "usage-a@example.test"), "", 0.60, "2026-10-05 16", sonnet),
		row(iamUser("usage-ci-bot"), "", 3.20, "2026-10-04 02", sonnet),
		row(sts("app-role", "session1"), "", 0.20, "2026-10-05 03", haiku),
		// Under a cent: left out of the list, counted in the total.
		row(iamUser("usage-tiny"), "", 0.004, "2026-10-05 04", haiku),
		// No caller: not a model call (for example S3 storage). Ignored.
		{Payer: acct, Code: "AmazonS3", UsageType: "TimedStorage-ByteHrs", Cost: 5.00, Start: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			Tags: map[string]string{}, Product: map[string]string{"product_name": "Amazon Simple Storage Service"}},
	}
}

func lastMonth() []fixtureRow {
	sso := "AWSReservedSSO_PowerUser_0123456789abcdef"
	return []fixtureRow{
		row(sts("bedrock-user-usage-a", "usage-a@example.test"), "usage-a@example.test", 30.00, "2026-09-10 09", sonnet),
		row(sts("bedrock-user-usage-a", "usage-a@example.test"), "usage-a@example.test", 25.00, "2026-09-12 09", sonnet),
		row(sts("bedrock-user-usage-b", "usage-b@example.test"), "usage-b@example.test", 20.00, "2026-09-15 09", haiku),
		row(sts(sso, "usage-sso@example.test"), "", 1.00, "2026-09-20 09", sonnet),
	}
}

func lastSnapshot() pause.Snapshot {
	return pause.Snapshot{
		Month: "2026-09", TakenAt: time.Date(2026, 10, 1, 6, 0, 5, 0, time.UTC), Account: acct,
		Users: []pause.SnapshotUser{
			{Email: "usage-a@example.test", Name: "usage-a", Budget: "bedrock-usage-a", Role: "bedrock-user-usage-a", LimitUSD: 50, PauseAtPercent: 100, TriggerUSD: 50,
				State: pause.Paused, PausedAt: time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)},
			{Email: "usage-b@example.test", Name: "usage-b", Budget: "bedrock-usage-b", Role: "bedrock-user-usage-b", LimitUSD: 100, PauseAtPercent: 100, TriggerUSD: 100,
				State: pause.Armed},
			{Email: "usage-c@example.test", Name: "usage-c", Budget: "bedrock-usage-c", Role: "bedrock-user-usage-c", LimitUSD: 30, PauseAtPercent: 100, TriggerUSD: 30,
				State: pause.PausedByHand, PausedAt: time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), By: "admin@example.test", Reason: "key leak"},
		},
	}
}

func writeParquet(t *testing.T, path string, rows []fixtureRow) {
	t.Helper()
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[fixtureRow](&buf)
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFixtures(t *testing.T) {
	if !*update {
		t.Skip("pass -update to rewrite tests/fixtures")
	}
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	writeParquet(t, filepath.Join(fixtures, "cur-this-month.parquet"), thisMonth())
	writeParquet(t, filepath.Join(fixtures, "cur-last-month.parquet"), lastMonth())
	b, _ := json.MarshalIndent(lastSnapshot(), "", "  ")
	if err := os.WriteFile(filepath.Join(fixtures, "snapshot-last-month.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// exportDir lays the fixtures out like the export: dir/data/BILLING_PERIOD=<month>/x.parquet.
func exportDir(t *testing.T, months map[string]string) DirSource {
	t.Helper()
	dir := t.TempDir()
	for month, file := range months {
		d := filepath.Join(dir, "data", "BILLING_PERIOD="+month)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(fixtures, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "part-0.parquet"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return DirSource{Dir: dir}
}

func readAll(t *testing.T, src Source, month string) []Line {
	t.Helper()
	var lines []Line
	n, err := ReadMonth(context.Background(), src, month, func(l Line) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("files: %d", n)
	}
	return lines
}

func TestReadFixture(t *testing.T) {
	src := exportDir(t, map[string]string{"2026-10": "cur-this-month.parquet"})
	lines := readAll(t, src, "2026-10")
	if len(lines) != 13 {
		t.Fatalf("lines: %d (the S3 row has no caller and must be skipped)", len(lines))
	}
	l := lines[0]
	if l.Owner != "usage-a@example.test" || l.Cost != 10 || l.Model != sonnet ||
		!l.Start.Equal(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)) || !strings.Contains(l.Principal, "bedrock-user-usage-a") {
		t.Fatalf("%+v", l)
	}
	if lines[6].Owner != "" {
		t.Fatalf("no owner tag: %+v", lines[6])
	}
}

func TestReadMissingMonth(t *testing.T) {
	src := exportDir(t, nil)
	n, err := ReadMonth(context.Background(), src, "2026-01", func(Line) {})
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func fixtureUsers() []User {
	return []User{
		{Email: "usage-a@example.test", LimitUSD: 50, TriggerUSD: 50, PauseKnown: true, Pause: pause.Status{State: pause.Armed}},
		{Email: "usage-b@example.test", LimitUSD: 100, TriggerUSD: 100, PauseKnown: true, Pause: pause.Status{State: pause.Off}},
		{Email: "usage-c@example.test", LimitUSD: 30, TriggerUSD: 30, PauseKnown: true,
			Pause: pause.Status{State: pause.PausedByHand, PausedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), Reason: "key leak"}},
		{Email: "usage-d@example.test", LimitUSD: 20, TriggerUSD: 20, PauseKnown: true, Pause: pause.Status{State: pause.Paused, PausedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}},
	}
}

func TestThisMonth(t *testing.T) {
	src := exportDir(t, map[string]string{"2026-10": "cur-this-month.parquet"})
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := Summarize("2026-10", readAll(t, src, "2026-10"), fixtureUsers(), now)
	if r.TrackedUSD != 18.45 || r.UntrackedUSD != 7.60 {
		t.Fatalf("totals: tracked %v untracked %v", r.TrackedUSD, r.UntrackedUSD)
	}
	a := r.Users[0]
	if a.SpentUSD != 13.25 || a.UsedPercent != 27 || a.ForecastUSD == nil || *a.ForecastUSD != 51 || a.PauseText != "armed" {
		t.Fatalf("a: %+v", a)
	}
	if b := r.Users[1]; b.ForecastUSD != nil || b.PauseText != "off until Nov 1" {
		t.Fatalf("b: %+v", b)
	}
	if c := r.Users[2]; c.PauseText != "PAUSED by hand (Oct 7: key leak)" || c.ForecastUSD == nil {
		t.Fatalf("c: %+v", c)
	}
	if d := r.Users[3]; d.SpentUSD != 0 || d.PauseText != "PAUSED (Oct 6)" || d.ForecastUSD != nil {
		t.Fatalf("d: %+v", d)
	}
	want := []Caller{
		{"usage-ci-bot", "IAM user", 3.20, "2026-10-04"},
		{"usage-sso@example.test", "SSO role PowerUser", 2.70, "2026-10-05"},
		{"usage-ghost@example.test", "personal role without budget: bedrock-user-usage-ghost", 0.80, "2026-10-04"},
		{"usage-a@example.test", "SSO role PowerUser", 0.60, "2026-10-05"},
		{"session1", "role app-role", 0.20, "2026-10-05"},
		{"usage-x@example.test", "personal role without owner tag: bedrock-user-usage-x", 0.10, "2026-10-04"},
	}
	if len(r.Untracked) != len(want) {
		t.Fatalf("callers: %+v", r.Untracked)
	}
	for i := range want {
		if r.Untracked[i] != want[i] {
			t.Errorf("caller %d: got %+v want %+v", i, r.Untracked[i], want[i])
		}
	}
	if !r.HasUntracked() {
		t.Fatal("HasUntracked")
	}
	var out bytes.Buffer
	r.Write(&out, Sections{true, true})
	for _, s := range []string{"2026-10 (to Oct 8, billing lags up to ~16 h)", "USERS", "FORECAST", "Tracked total", "$18.45",
		"UNTRACKED (outside personal roles)", "usage-ci-bot", "Untracked total", "$7.60"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("missing %q in\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "usage-tiny") {
		t.Error("callers under $0.01 must be left out")
	}
	out.Reset()
	r.Write(&out, Sections{Untracked: true})
	if strings.Contains(out.String(), "USERS") {
		t.Error("--untracked shows users")
	}
}

func TestNoUntracked(t *testing.T) {
	r := Summarize("2026-10", []Line{{Principal: sts("bedrock-user-a", "a@x.test"), Owner: "a@x.test", Cost: 1}},
		[]User{{Email: "a@x.test", LimitUSD: 10, PauseKnown: true, Pause: pause.Status{State: pause.Armed}}}, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	var out bytes.Buffer
	r.Write(&out, Sections{true, true})
	if r.HasUntracked() || !strings.Contains(out.String(), "Everyone used their personal role. Nothing to follow up.") {
		t.Fatal(out.String())
	}
}

func TestPastMonthFromSnapshot(t *testing.T) {
	src := exportDir(t, map[string]string{"2026-09": "cur-last-month.parquet"})
	b, err := os.ReadFile(filepath.Join(fixtures, "snapshot-last-month.json"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := pause.ParseSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	r := Summarize("2026-09", readAll(t, src, "2026-09"), UsersFromSnapshot(snap), time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	if r.Current || r.TrackedUSD != 75 || r.UntrackedUSD != 1 {
		t.Fatalf("%+v", r)
	}
	if a := r.Users[0]; a.SpentUSD != 55 || a.UsedPercent != 110 || a.PauseText != "PAUSED (Sep 12)" || a.ForecastUSD != nil {
		t.Fatalf("a: %+v", a)
	}
	if c := r.Users[2]; c.PauseText != "PAUSED by hand (Sep 20: key leak)" || c.SpentUSD != 0 {
		t.Fatalf("c: %+v", c)
	}
	var out bytes.Buffer
	r.Write(&out, Sections{true, true})
	if strings.Contains(out.String(), "FORECAST") || !strings.Contains(out.String(), "2026-09 (final)") {
		t.Fatal(out.String())
	}
}

func TestMonths(t *testing.T) {
	src := exportDir(t, map[string]string{"2026-09": "cur-last-month.parquet", "2026-10": "cur-this-month.parquet"})
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	us := fixtureUsers()
	m := Combine([]MonthReport{
		Summarize("2026-09", readAll(t, src, "2026-09"), us, now),
		Summarize("2026-10", readAll(t, src, "2026-10"), us, now),
	})
	if len(m.Months) != 2 || m.Users[0].Who != "usage-a@example.test" || m.Users[0].TotalUSD != 68.25 {
		t.Fatalf("%+v", m.Users)
	}
	var sso Series
	for _, s := range m.Untracked {
		if s.Who == "usage-sso@example.test" {
			sso = s
		}
	}
	if sso.TotalUSD != 3.70 || sso.ByMonth["2026-09"] != 1 || sso.ByMonth["2026-10"] != 2.70 {
		t.Fatalf("sso: %+v", sso)
	}
	var out bytes.Buffer
	m.Write(&out, Sections{true, true})
	for _, s := range []string{"2026-09", "2026-10", "TOTAL", "68.25", "Tracked total", "93.45", "Untracked total"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("missing %q in\n%s", s, out.String())
		}
	}
	// Names are left-aligned (numbers right-aligned).
	for _, line := range strings.Split(out.String(), "\n") {
		for _, name := range []string{"USERS", "usage-a@example.test", "Tracked total"} {
			if strings.Contains(line, name) && !strings.HasPrefix(line, name) {
				t.Errorf("%q not left-aligned: %q", name, line)
			}
		}
	}
}

func TestPerson(t *testing.T) {
	src := exportDir(t, map[string]string{"2026-10": "cur-this-month.parquet"})
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	lines := readAll(t, src, "2026-10")
	r := Summarize("2026-10", lines, fixtureUsers(), now)

	d := Person(r, lines, "usage-a@example.test")
	if d.User == nil || d.TotalUSD != 13.85 {
		t.Fatalf("%+v", d)
	}
	wantDays := []Amount{{"2026-10-01", 10}, {"2026-10-02", 3.25}, {"2026-10-05", 0.60}}
	if len(d.Days) != 3 || d.Days[0] != wantDays[0] || d.Days[1] != wantDays[1] || d.Days[2] != wantDays[2] {
		t.Fatalf("days %+v", d.Days)
	}
	if d.Models[0] != (Amount{sonnet, 11.35}) || d.Models[1] != (Amount{haiku, 2.50}) {
		t.Fatalf("models %+v", d.Models)
	}
	if len(d.Callers) != 2 || d.Callers[0].UsedVia != "personal role" || d.Callers[1].UsedVia != "SSO role PowerUser" {
		t.Fatalf("callers %+v", d.Callers)
	}

	d = Person(r, lines, "usage-sso@example.test")
	if d.User != nil || d.TotalUSD != 2.70 || len(d.Days) != 2 {
		t.Fatalf("%+v", d)
	}
	var out bytes.Buffer
	d.Write(&out)
	if !strings.Contains(out.String(), "Not a user with a budget") || !strings.Contains(out.String(), "2026-10-05") {
		t.Fatal(out.String())
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct{ arn, owner, who, via string }{
		{sts("AWSReservedSSO_Admin_abc123", "x@y.z"), "", "x@y.z", "SSO role Admin"},
		{sts("AWSReservedSSO_Bedrock_Users_abc123", "x@y.z"), "", "x@y.z", "SSO role Bedrock_Users"},
		{sts("bedrock-user-x", "x@y.z"), "", "x@y.z", "personal role without owner tag: bedrock-user-x"},
		{sts("bedrock-user-x", "x@y.z"), "x@y.z", "x@y.z", "personal role without budget: bedrock-user-x"},
		{sts("app", "s"), "", "s", "role app"},
		{iamUser("bot"), "", "bot", "IAM user"},
		{"arn:aws:iam::1:root", "", "arn:aws:iam::1:root", "arn:aws:iam::1:root"},
	} {
		who, via := Classify(c.arn, c.owner)
		if who != c.who || via != c.via {
			t.Errorf("%s: got %q %q", c.arn, who, via)
		}
	}
}
