// Package users plans and applies the users: section (personal roles, budgets, pause actions).
package users

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/pause"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
)

// Observed is a user's current state in AWS, in plain values so Diff can be tested.
type Observed struct {
	RoleExists bool
	RoleID     string
	RoleDrift  []string

	BudgetExists  bool
	BudgetID      string
	Limit         float64
	FilterOK      bool
	Spend         float64
	Forecast      float64
	Alerts        map[int][]string // alert threshold % → subscriber addresses
	ExtraNotified int              // notifications that aren't alerts bedrock-admin makes

	ActionExists      bool
	ActionID          string
	ActionShapeOK     bool // type, policy, role, execution role, approval, ACTUAL, PERCENTAGE
	ActionThreshold   int
	ActionSubscribers []string

	Pause pause.Status
}

// Change is what apply must do for one user, and how plan describes it.
type Change struct {
	Op       plan.Op
	Details  []string
	Notes    []string
	NeedsYes bool

	CreateRole, FixRole                 bool
	CreateBudget, UpdateBudget          bool
	CreateAction, UpdateAction          bool
	RecreateAction                      bool
	AddAlerts, RemoveAlerts, ResubAlert []int
	Rearm                               bool
	RearmReason                         string
}

// USD formats an amount the way plan and usage show it: 50, 52.10.
func USD(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// Money formats an amount with a dollar sign and cents: $52.10.
func Money(v float64) string { return "$" + strconv.FormatFloat(v, 'f', 2, 64) }

func alertList(a []int) string {
	if len(a) == 0 {
		return "none"
	}
	s := make([]string, len(a))
	for i, v := range a {
		s[i] = strconv.Itoa(v) + "%"
	}
	return strings.Join(s, " ")
}

// Diff compares a configured user with what exists. id is this config's id.
func Diff(want config.Resolved, o Observed, id string, now time.Time) Change {
	var c Change
	if o.RoleExists && o.RoleID != id {
		c.Op = plan.Conflict
		if o.RoleID == "" {
			c.Details = []string{"role " + want.RoleName + " exists but was not created by bedrock-admin; left alone"}
		} else {
			c.Details = []string{"role " + want.RoleName + " is managed by " + o.RoleID + "; left alone"}
		}
		return c
	}
	if o.BudgetExists && o.BudgetID != id {
		c.Op = plan.Conflict
		owner := "was not created by bedrock-admin"
		if o.BudgetID != "" {
			owner = "is managed by " + o.BudgetID
		}
		c.Details = []string{"budget " + want.BudgetName + " " + owner + "; left alone"}
		return c
	}
	if !o.RoleExists {
		c.Op, c.CreateRole = plan.Create, true
		c.Details = []string{"onboard, limit " + USD(want.LimitUSD)}
		if !o.BudgetExists {
			c.CreateBudget, c.CreateAction = true, true
			return c
		}
	} else if len(o.RoleDrift) > 0 {
		c.FixRole = true
		c.Details = append(c.Details, o.RoleDrift...)
	}
	if !o.BudgetExists {
		c.CreateBudget, c.CreateAction = true, true
		c.Details = append(c.Details, "budget missing, create it")
		c.Notes = append(c.Notes, "new pause action starts armed")
		c.Op = plan.Update
		return c
	}

	limitChanged := o.Limit != want.LimitUSD
	if limitChanged {
		c.UpdateBudget = true
		c.Details = append(c.Details, "limit "+USD(o.Limit)+" → "+USD(want.LimitUSD))
	}
	if !o.FilterOK {
		c.UpdateBudget = true
		c.Details = append(c.Details, "budget filter changed outside the file")
	}

	have := make([]int, 0, len(o.Alerts))
	for p := range o.Alerts {
		have = append(have, p)
	}
	sort.Ints(have)
	if !slices.Equal(have, want.AlertAtPercent) {
		c.Details = append(c.Details, "alerts "+alertList(have)+" → "+alertList(want.AlertAtPercent))
	}
	notifyChanged := false
	for _, p := range want.AlertAtPercent {
		subs, ok := o.Alerts[p]
		if !ok {
			c.AddAlerts = append(c.AddAlerts, p)
		} else if !slices.Equal(subs, []string{want.Notify}) {
			c.ResubAlert = append(c.ResubAlert, p)
			notifyChanged = true
		}
	}
	for _, p := range have {
		if !slices.Contains(want.AlertAtPercent, p) {
			c.RemoveAlerts = append(c.RemoveAlerts, p)
		}
	}

	thresholdChanged := false
	switch {
	case !o.ActionExists:
		c.CreateAction = true
		c.Details = append(c.Details, "pause action missing, create it")
		c.Notes = append(c.Notes, "new pause action starts armed")
	case !o.ActionShapeOK:
		c.RecreateAction, c.NeedsYes = true, true
		c.Details = append(c.Details, "pause action changed outside the file, recreate it")
		c.Notes = append(c.Notes, "recreating re-arms it")
	default:
		if o.ActionThreshold != want.PauseAtPercent {
			thresholdChanged, c.UpdateAction = true, true
			c.Details = append(c.Details, fmt.Sprintf("pause_at %d%% → %d%%", o.ActionThreshold, want.PauseAtPercent))
		}
		if !slices.Equal(o.ActionSubscribers, []string{want.Notify}) {
			c.UpdateAction, notifyChanged = true, true
		}
	}
	if notifyChanged {
		from := strings.Join(o.ActionSubscribers, ",")
		if from == "" && len(have) > 0 {
			from = strings.Join(o.Alerts[have[0]], ",")
		}
		c.Details = append(c.Details, "notify "+from+" → "+want.Notify)
	}

	if (limitChanged || thresholdChanged) && o.ActionExists && o.ActionShapeOK {
		c.Notes = append(c.Notes, triggerNotes(&c, want, o, limitChanged, thresholdChanged, now)...)
	}

	if len(c.Details) > 0 && c.Op == "" {
		c.Op = plan.Update
	}
	if c.Op == "" {
		c.Op = plan.OK
	}
	return c
}

// triggerNotes applies the "Limit changes" table.
func triggerNotes(c *Change, want config.Resolved, o Observed, limitChanged, thresholdChanged bool, now time.Time) []string {
	trigger := want.TriggerUSD()
	spent := Money(o.Spend)
	off := "pause off until " + pause.NextMonth(now).Format("Jan 2")
	withTrigger := func(s string) string {
		sep := ", "
		if s == "still over" {
			sep = ": "
		}
		if thresholdChanged {
			return s + sep + "trigger " + Money(trigger) + ", spent " + spent
		}
		return s + sep + "spent " + spent
	}
	switch o.Pause.State {
	case pause.Armed:
		if trigger <= o.Spend {
			return []string{"warning: spent " + spent + ", will be paused"}
		}
	case pause.Paused:
		if trigger > o.Spend {
			c.Rearm = true
			if limitChanged {
				c.RearmReason = "limit raised to $" + USD(want.LimitUSD) + " in bedrock.yaml"
			} else {
				c.RearmReason = fmt.Sprintf("pause_at_percent raised to %d%% in bedrock.yaml", want.PauseAtPercent)
			}
			return []string{withTrigger("paused → unpaused")}
		}
		return []string{withTrigger("still over") + ", stays paused"}
	case pause.PausedByHand:
		return []string{"paused by hand, unchanged"}
	case pause.Off:
		if trigger <= o.Spend {
			return []string{off + ", unchanged", "warning: " + off + "; spending is not capped"}
		}
		return []string{off + ", unchanged"}
	}
	return nil
}
