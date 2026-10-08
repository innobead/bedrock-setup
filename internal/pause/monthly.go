package pause

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// Snapshot records every user's limit and pause state at the end of a month. The monthly Lambda
// writes it before re-arming; usage reads it for past months.
type Snapshot struct {
	Month   string         `json:"month"`
	TakenAt time.Time      `json:"taken_at"`
	Account string         `json:"account"`
	Users   []SnapshotUser `json:"users"`
}

type SnapshotUser struct {
	Email          string    `json:"email"`
	Name           string    `json:"name"`
	Budget         string    `json:"budget"`
	Role           string    `json:"role"`
	LimitUSD       float64   `json:"limit_usd"`
	PauseAtPercent int       `json:"pause_at_percent"`
	TriggerUSD     float64   `json:"trigger_usd"`
	State          State     `json:"state"`
	PausedAt       time.Time `json:"paused_at,omitzero"`
	By             string    `json:"paused_by,omitempty"`
	Reason         string    `json:"reason,omitempty"`
}

// Find returns the entry for an email.
func (s *Snapshot) Find(email string) (SnapshotUser, bool) {
	for _, u := range s.Users {
		if strings.EqualFold(u.Email, email) {
			return u, true
		}
	}
	return SnapshotUser{}, false
}

// SnapshotKey is the S3 key of a month's snapshot.
func SnapshotKey(prefix, month string) string {
	return strings.Trim(prefix, "/") + "/" + month + ".json"
}

// PreviousMonth is the YYYY-MM before the month of t (UTC).
func PreviousMonth(t time.Time) string {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()-1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
}

// RoleForBudget maps a budget name to its personal role by convention: bedrock-X ↔ bedrock-user-X.
func RoleForBudget(budget string) string {
	return "bedrock-user-" + strings.TrimPrefix(budget, "bedrock-")
}

// MonthlyResult says what the monthly run did for each user.
type MonthlyResult struct {
	Snapshot Snapshot          `json:"snapshot"`
	Done     map[string]string `json:"done"`
	Errors   []string          `json:"errors,omitempty"`
}

func (m *Manager) pauseActions(ctx context.Context) ([]btypes.Action, error) {
	var out []btypes.Action
	var token *string
	for {
		r, err := m.Budgets.DescribeBudgetActionsForAccount(ctx, &budgets.DescribeBudgetActionsForAccountInput{
			AccountId: aws.String(m.Account), NextToken: token})
		if err != nil {
			return nil, err
		}
		for _, a := range r.Actions {
			if IsPauseAction(a, m.Account) && strings.HasPrefix(aws.ToString(a.BudgetName), "bedrock-") {
				out = append(out, a)
			}
		}
		if r.NextToken == nil {
			break
		}
		token = r.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return aws.ToString(out[i].BudgetName) < aws.ToString(out[j].BudgetName) })
	return out, nil
}

func (m *Manager) limits(ctx context.Context) (map[string]float64, error) {
	out := map[string]float64{}
	var token *string
	for {
		r, err := m.Budgets.DescribeBudgets(ctx, &budgets.DescribeBudgetsInput{AccountId: aws.String(m.Account), NextToken: token})
		if err != nil {
			return nil, err
		}
		for _, b := range r.Budgets {
			if b.BudgetLimit != nil {
				v, _ := strconv.ParseFloat(aws.ToString(b.BudgetLimit.Amount), 64)
				out[aws.ToString(b.BudgetName)] = v
			}
		}
		if r.NextToken == nil {
			return out, nil
		}
		token = r.NextToken
	}
}

// TakeSnapshot reads every user's limit and pause state.
func (m *Manager) TakeSnapshot(ctx context.Context, month string) (Snapshot, []btypes.Action, map[string]map[string]string, error) {
	s := Snapshot{Month: month, TakenAt: m.now().UTC(), Account: m.Account, Users: []SnapshotUser{}}
	actions, err := m.pauseActions(ctx)
	if err != nil {
		return s, nil, nil, fmt.Errorf("listing budget actions: %w", err)
	}
	limits, err := m.limits(ctx)
	if err != nil {
		return s, nil, nil, fmt.Errorf("listing budgets: %w", err)
	}
	tags := map[string]map[string]string{}
	for _, a := range actions {
		budget := aws.ToString(a.BudgetName)
		role := RoleForBudget(budget)
		t, err := m.RoleTags(ctx, role)
		if err != nil && !isNoSuchEntity(err) {
			return s, nil, nil, fmt.Errorf("reading tags of %s: %w", role, err)
		}
		tags[role] = t
		st := Derive(string(a.Status), t)
		if st.State == Paused {
			st.PausedAt = m.lastExecution(ctx, budget, aws.ToString(a.ActionId))
		}
		pct := 0
		if a.ActionThreshold != nil {
			pct = int(a.ActionThreshold.ActionThresholdValue)
		}
		limit := limits[budget]
		s.Users = append(s.Users, SnapshotUser{Email: t[policy.OwnerTag], Name: strings.TrimPrefix(budget, "bedrock-"),
			Budget: budget, Role: role, LimitUSD: limit, PauseAtPercent: pct, TriggerUSD: float64(int64(limit*float64(pct)+0.5)) / 100,
			State: st.State, PausedAt: st.PausedAt, By: st.By, Reason: st.Reason})
	}
	return s, actions, tags, nil
}

// Monthly runs on the 1st: saves the snapshot of the month that ended (via save), then re-arms every
// pause action that is paused or off, and keeps pauses by hand in place. Errors for one user do not
// stop the others; a failed snapshot does not stop the re-arming (nobody stays locked out because
// S3 failed), but makes the run fail.
func (m *Manager) Monthly(ctx context.Context, save func(context.Context, Snapshot) error) (MonthlyResult, error) {
	res := MonthlyResult{Done: map[string]string{}}
	snap, actions, tags, err := m.TakeSnapshot(ctx, PreviousMonth(m.now()))
	if err != nil {
		return res, err
	}
	res.Snapshot = snap
	if err := save(ctx, snap); err != nil {
		res.Errors = append(res.Errors, "saving the snapshot: "+err.Error())
	}
	for _, a := range actions {
		budget, id := aws.ToString(a.BudgetName), aws.ToString(a.ActionId)
		var err error
		switch a.Status {
		case btypes.ActionStatusExecutionSuccess:
			if err = m.ReverseAndWait(ctx, budget, id); err == nil {
				err = m.Reset(ctx, budget, id)
			}
			res.Done[budget] = "re-armed (was paused)"
		case btypes.ActionStatusReverseSuccess:
			err = m.Reset(ctx, budget, id)
			res.Done[budget] = "re-armed (was off)"
		default:
			res.Done[budget] = "unchanged (" + string(a.Status) + ")"
		}
		if err != nil {
			res.Done[budget] = "failed"
			res.Errors = append(res.Errors, err.Error())
		}
		role := RoleForBudget(budget)
		if _, ok := tags[role][TagPausedBy]; ok {
			// Reversing the action detaches bedrock-deny, even from a role paused by hand.
			if _, err := m.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{RoleName: aws.String(role),
				PolicyArn: aws.String(policy.DenyPolicyArn(m.Account))}); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("keeping the pause by hand of %s: %v", role, err))
			} else {
				res.Done[budget] += "; still paused by hand"
			}
		}
	}
	if len(res.Errors) > 0 {
		return res, errors.New(strings.Join(res.Errors, "; "))
	}
	return res, nil
}

// ParseSnapshot decodes a snapshot file.
func ParseSnapshot(b []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("reading snapshot: %w", err)
	}
	return &s, nil
}
