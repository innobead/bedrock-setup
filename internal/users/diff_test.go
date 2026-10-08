package users

import (
	"strings"
	"testing"
	"time"

	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/pause"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func want(limit float64, pauseAt int) config.Resolved {
	return config.Resolved{Email: "dave@acme.com", Name: "dave", RoleName: "bedrock-user-dave", BudgetName: "bedrock-dave",
		LimitUSD: limit, PauseAtPercent: pauseAt, AlertAtPercent: []int{80}, Notify: "dave@acme.com"}
}

func observed(limit float64, pauseAt int, spend float64, st pause.State) Observed {
	return Observed{RoleExists: true, RoleID: "acme", BudgetExists: true, BudgetID: "acme", Limit: limit, FilterOK: true,
		Spend: spend, Alerts: map[int][]string{80: {"dave@acme.com"}}, ActionExists: true, ActionID: "a1",
		ActionShapeOK: true, ActionThreshold: pauseAt, ActionSubscribers: []string{"dave@acme.com"}, Pause: pause.Status{State: st}}
}

func line(c Change) string {
	s := strings.Join(c.Details, ", ")
	if len(c.Notes) > 0 {
		s += "  (" + strings.Join(c.Notes, "; ") + ")"
	}
	return s
}

func TestLimitChanges(t *testing.T) {
	cases := []struct {
		name  string
		want  config.Resolved
		obs   Observed
		line  string
		rearm bool
	}{
		{"armed", want(80, 100), observed(50, 100, 10, pause.Armed), "limit 50 → 80", false},
		{"armed below spend", want(40, 100), observed(50, 100, 45, pause.Armed), "limit 50 → 40  (warning: spent $45.00, will be paused)", false},
		{"paused, raised above", want(80, 100), observed(50, 100, 52, pause.Paused), "limit 50 → 80  (paused → unpaused, spent $52.00)", true},
		{"paused, still over", want(52, 100), observed(50, 100, 52.10, pause.Paused), "limit 50 → 52  (still over: spent $52.10, stays paused)", false},
		{"pause_at raised", want(50, 110), observed(50, 100, 52.10, pause.Paused), "pause_at 100% → 110%  (paused → unpaused, trigger $55.00, spent $52.10)", true},
		{"by hand", want(80, 100), observed(50, 100, 10, pause.PausedByHand), "limit 50 → 80  (paused by hand, unchanged)", false},
		{"off", want(60, 100), observed(30, 100, 30.90, pause.Off), "limit 30 → 60  (pause off until Nov 1, unchanged)", false},
		{"off below spend", want(20, 100), observed(30, 100, 30.90, pause.Off), "limit 30 → 20  (pause off until Nov 1, unchanged; warning: pause off until Nov 1; spending is not capped)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Diff(tc.want, tc.obs, "acme", now)
			if c.Op != plan.Update {
				t.Errorf("op = %s", c.Op)
			}
			if got := line(c); got != tc.line {
				t.Errorf("got  %q\nwant %q", got, tc.line)
			}
			if c.Rearm != tc.rearm {
				t.Errorf("rearm = %v", c.Rearm)
			}
			if c.CreateAction || c.RecreateAction {
				t.Error("a limit change must keep the same action")
			}
		})
	}
}

func TestNoChange(t *testing.T) {
	c := Diff(want(50, 100), observed(50, 100, 10, pause.Armed), "acme", now)
	if c.Op != plan.OK || len(c.Details) != 0 {
		t.Fatalf("got %s %v", c.Op, c.Details)
	}
}

func TestOnboard(t *testing.T) {
	c := Diff(want(50, 100), Observed{}, "acme", now)
	if c.Op != plan.Create || !c.CreateRole || !c.CreateBudget || !c.CreateAction || line(c) != "onboard, limit 50" {
		t.Fatalf("got %+v", c)
	}
}

func TestOtherID(t *testing.T) {
	o := observed(50, 100, 0, pause.Armed)
	o.RoleID = "other"
	if c := Diff(want(80, 100), o, "acme", now); c.Op != plan.Conflict || c.UpdateBudget {
		t.Fatalf("got %+v", c)
	}
	o.RoleID = ""
	if c := Diff(want(80, 100), o, "acme", now); c.Op != plan.Conflict {
		t.Fatalf("got %+v", c)
	}
}

func TestAlertsAndNotify(t *testing.T) {
	w := want(50, 100)
	w.AlertAtPercent = []int{50, 90}
	w.Notify = "lead@acme.com"
	c := Diff(w, observed(50, 100, 0, pause.Armed), "acme", now)
	if got := line(c); got != "alerts 80% → 50% 90%, notify dave@acme.com → lead@acme.com" {
		t.Fatalf("got %q", got)
	}
	if len(c.AddAlerts) != 2 || len(c.RemoveAlerts) != 1 || !c.UpdateAction {
		t.Fatalf("got %+v", c)
	}
	w = want(50, 100)
	w.AlertAtPercent = []int{}
	c = Diff(w, observed(50, 100, 0, pause.Armed), "acme", now)
	if got := line(c); got != "alerts 80% → none" {
		t.Fatalf("got %q", got)
	}
}

func TestRecreateNeedsYes(t *testing.T) {
	o := observed(50, 100, 0, pause.Armed)
	o.ActionShapeOK = false
	c := Diff(want(50, 100), o, "acme", now)
	if !c.NeedsYes || !c.RecreateAction {
		t.Fatalf("got %+v", c)
	}
}

func TestMissingBudget(t *testing.T) {
	o := Observed{RoleExists: true, RoleID: "acme"}
	c := Diff(want(50, 100), o, "acme", now)
	if c.Op != plan.Update || !c.CreateBudget || !strings.Contains(line(c), "new pause action starts armed") {
		t.Fatalf("got %+v", c)
	}
}

func TestSetupCommand(t *testing.T) {
	cfg := &config.Config{Region: "us-west-2"}
	u := want(50, 100)
	if got := SetupCommand(cfg, "123456789012", "us-west-2", u); got != "bedrock setup" {
		t.Fatalf("got %q", got)
	}
	u.CustomName, u.RoleName = true, "bedrock-user-dv"
	got := SetupCommand(cfg, "123456789012", "eu-west-1", u)
	if got != "bedrock setup --region us-west-2 --role-arn arn:aws:iam::123456789012:role/bedrock-users/bedrock-user-dv" {
		t.Fatalf("got %q", got)
	}
}
