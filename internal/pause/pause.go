// Package pause holds the pause state model and the operations that change it. The CLI and the
// monthly Lambda share it.
package pause

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	itypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// Tags on a personal role that record runtime actions.
const (
	TagPausedBy      = "bedrock:paused-by"
	TagPausedAt      = "bedrock:paused-at"
	TagPauseReason   = "bedrock:pause-reason"
	TagUnpausedBy    = "bedrock:unpaused-by"
	TagUnpausedAt    = "bedrock:unpaused-at"
	TagUnpauseReason = "bedrock:unpause-reason"
)

type State string

const (
	Armed        State = "armed"
	Paused       State = "paused"
	PausedByHand State = "paused-by-hand"
	Off          State = "off"
	Busy         State = "in-progress"
	Failed       State = "failed"
	NoAction     State = "no-action"
)

// Status is a user's pause state, derived from the budget action and the role's tags.
type Status struct {
	State        State     `json:"state"`
	ActionStatus string    `json:"action_status,omitempty"`
	ActionID     string    `json:"action_id,omitempty"`
	PausedAt     time.Time `json:"paused_at,omitzero"`
	By           string    `json:"paused_by,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	// Underlying is the action's state for a user paused by hand.
	Underlying State `json:"underlying,omitempty"`
}

// FromAction maps a budget action status to a state.
func FromAction(status string) State {
	switch btypes.ActionStatus(status) {
	case btypes.ActionStatusStandby, btypes.ActionStatusPending:
		return Armed
	case btypes.ActionStatusExecutionSuccess:
		return Paused
	case btypes.ActionStatusReverseSuccess:
		return Off
	case btypes.ActionStatusExecutionInProgress, btypes.ActionStatusReverseInProgress, btypes.ActionStatusResetInProgress:
		return Busy
	case btypes.ActionStatusExecutionFailure, btypes.ActionStatusReverseFailure, btypes.ActionStatusResetFailure:
		return Failed
	case "":
		return NoAction
	}
	return Failed
}

// Derive combines the action status and the role tags. A pause by hand wins over the action.
func Derive(actionStatus string, tags map[string]string) Status {
	s := Status{State: FromAction(actionStatus), ActionStatus: actionStatus}
	if by, ok := tags[TagPausedBy]; ok {
		s.Underlying = s.State
		s.State = PausedByHand
		s.By = by
		s.Reason = tags[TagPauseReason]
		if t, err := time.Parse(time.RFC3339, tags[TagPausedAt]); err == nil {
			s.PausedAt = t
		}
	}
	return s
}

// NextMonth is the 1st of the month after t, in UTC.
func NextMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// Text renders a state the way usage and plan show it.
func (s Status) Text(now time.Time) string {
	switch s.State {
	case Armed:
		return "armed"
	case Paused:
		if s.PausedAt.IsZero() {
			return "PAUSED"
		}
		return "PAUSED (" + s.PausedAt.UTC().Format("Jan 2") + ")"
	case PausedByHand:
		var parts []string
		if !s.PausedAt.IsZero() {
			parts = append(parts, s.PausedAt.UTC().Format("Jan 2"))
		}
		if s.Reason != "" {
			parts = append(parts, s.Reason)
		}
		if len(parts) == 0 {
			return "PAUSED by hand"
		}
		return "PAUSED by hand (" + strings.Join(parts, ": ") + ")"
	case Off:
		return "off until " + NextMonth(now).Format("Jan 2")
	case Busy:
		return "changing (" + s.ActionStatus + ")"
	case Failed:
		return "ERROR (" + s.ActionStatus + ")"
	case NoAction:
		return "no pause action"
	}
	return string(s.State)
}

// BudgetsAPI is the part of the AWS Budgets client this package uses.
type BudgetsAPI interface {
	DescribeBudgetAction(context.Context, *budgets.DescribeBudgetActionInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetActionOutput, error)
	ExecuteBudgetAction(context.Context, *budgets.ExecuteBudgetActionInput, ...func(*budgets.Options)) (*budgets.ExecuteBudgetActionOutput, error)
	DescribeBudgetActionsForBudget(context.Context, *budgets.DescribeBudgetActionsForBudgetInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetActionsForBudgetOutput, error)
	DescribeBudgetActionsForAccount(context.Context, *budgets.DescribeBudgetActionsForAccountInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetActionsForAccountOutput, error)
	DescribeBudgetActionHistories(context.Context, *budgets.DescribeBudgetActionHistoriesInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetActionHistoriesOutput, error)
	DescribeBudgets(context.Context, *budgets.DescribeBudgetsInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetsOutput, error)
}

// IAMAPI is the part of the IAM client this package uses.
type IAMAPI interface {
	ListRoleTags(context.Context, *iam.ListRoleTagsInput, ...func(*iam.Options)) (*iam.ListRoleTagsOutput, error)
	TagRole(context.Context, *iam.TagRoleInput, ...func(*iam.Options)) (*iam.TagRoleOutput, error)
	UntagRole(context.Context, *iam.UntagRoleInput, ...func(*iam.Options)) (*iam.UntagRoleOutput, error)
	AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
}

// Manager changes pause state.
type Manager struct {
	Budgets BudgetsAPI
	IAM     IAMAPI
	Account string
	// Poll and Wait control how long a reverse or reset is waited for.
	Poll  time.Duration
	Wait  time.Duration
	Now   func() time.Time
	Sleep func(time.Duration)
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) sleep(d time.Duration) {
	if m.Sleep != nil {
		m.Sleep(d)
		return
	}
	time.Sleep(d)
}

// RoleTags returns a role's tags as a map.
func (m *Manager) RoleTags(ctx context.Context, role string) (map[string]string, error) {
	out := map[string]string{}
	var marker *string
	for {
		r, err := m.IAM.ListRoleTags(ctx, &iam.ListRoleTagsInput{RoleName: aws.String(role), Marker: marker})
		if err != nil {
			return nil, err
		}
		for _, t := range r.Tags {
			out[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		if !r.IsTruncated {
			return out, nil
		}
		marker = r.Marker
	}
}

// Action returns the budget's pause action (the one that attaches bedrock-deny), or nil.
func (m *Manager) Action(ctx context.Context, budget string) (*btypes.Action, error) {
	r, err := m.Budgets.DescribeBudgetActionsForBudget(ctx, &budgets.DescribeBudgetActionsForBudgetInput{
		AccountId: aws.String(m.Account), BudgetName: aws.String(budget)})
	if err != nil {
		var nf *btypes.NotFoundException
		if errors.As(err, &nf) {
			return nil, nil
		}
		return nil, err
	}
	for i := range r.Actions {
		if IsPauseAction(r.Actions[i], m.Account) {
			return &r.Actions[i], nil
		}
	}
	return nil, nil
}

// IsPauseAction reports whether an action attaches bedrock-deny.
func IsPauseAction(a btypes.Action, account string) bool {
	return a.ActionType == btypes.ActionTypeIam && a.Definition != nil && a.Definition.IamActionDefinition != nil &&
		aws.ToString(a.Definition.IamActionDefinition.PolicyArn) == policy.DenyPolicyArn(account)
}

// Status reads a user's current pause state.
func (m *Manager) Status(ctx context.Context, role, budget string) (Status, error) {
	tags, err := m.RoleTags(ctx, role)
	if err != nil {
		return Status{}, err
	}
	a, err := m.Action(ctx, budget)
	if err != nil {
		return Status{}, err
	}
	st := ""
	id := ""
	if a != nil {
		st, id = string(a.Status), aws.ToString(a.ActionId)
	}
	s := Derive(st, tags)
	s.ActionID = id
	if s.State == Paused {
		s.PausedAt = m.lastExecution(ctx, budget, id)
	}
	return s, nil
}

// lastExecution finds when the action last paused the user; zero if unknown.
func (m *Manager) lastExecution(ctx context.Context, budget, id string) time.Time {
	r, err := m.Budgets.DescribeBudgetActionHistories(ctx, &budgets.DescribeBudgetActionHistoriesInput{
		AccountId: aws.String(m.Account), BudgetName: aws.String(budget), ActionId: aws.String(id)})
	if err != nil {
		return time.Time{}
	}
	var last time.Time
	for _, h := range r.ActionHistories {
		if h.Status == btypes.ActionStatusExecutionSuccess && h.Timestamp != nil && h.Timestamp.After(last) {
			last = *h.Timestamp
		}
	}
	return last
}

var tagUnsafe = regexp.MustCompile(`[^\p{L}\p{Z}\p{N}_.:/=+\-@]+`)

// TagValue makes text safe for an IAM tag value (256 characters, limited character set).
func TagValue(s string) string {
	s = strings.TrimSpace(tagUnsafe.ReplaceAllString(s, " "))
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}

// Pause pauses a user by hand: attaches bedrock-deny and records who, when and why.
func (m *Manager) Pause(ctx context.Context, role, by, reason string) error {
	if _, err := m.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{RoleName: aws.String(role),
		PolicyArn: aws.String(policy.DenyPolicyArn(m.Account))}); err != nil {
		return fmt.Errorf("attaching %s to %s: %w", policy.DenyPolicyName, role, err)
	}
	_, err := m.IAM.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(role), Tags: []itypes.Tag{
		{Key: aws.String(TagPausedBy), Value: aws.String(TagValue(by))},
		{Key: aws.String(TagPausedAt), Value: aws.String(m.now().UTC().Format(time.RFC3339))},
		{Key: aws.String(TagPauseReason), Value: aws.String(TagValue(reason))},
	}})
	return err
}

// Result says what an unpause did.
type Result string

const (
	NotPaused        Result = "not-paused"
	RemovedManual    Result = "removed-manual-pause"
	TurnedOff        Result = "off-until-1st"
	Rearmed          Result = "rearmed"
	StillPausedByAWS Result = "removed-manual-still-over-limit"
)

// Unpause restores access. A pause by hand is removed (the action is left as it is); a pause by
// AWS Budgets is reversed, which leaves the pause off until the 1st.
func (m *Manager) Unpause(ctx context.Context, role, budget, by, reason string) (Result, error) {
	s, err := m.Status(ctx, role, budget)
	if err != nil {
		return "", err
	}
	switch s.State {
	case PausedByHand:
		if s.Underlying != Paused {
			if _, err := m.IAM.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(role),
				PolicyArn: aws.String(policy.DenyPolicyArn(m.Account))}); err != nil && !isNoSuchEntity(err) {
				return "", fmt.Errorf("detaching %s from %s: %w", policy.DenyPolicyName, role, err)
			}
		}
		if _, err := m.IAM.UntagRole(ctx, &iam.UntagRoleInput{RoleName: aws.String(role),
			TagKeys: []string{TagPausedBy, TagPausedAt, TagPauseReason}}); err != nil {
			return "", err
		}
		if err := m.record(ctx, role, by, reason); err != nil {
			return "", err
		}
		if s.Underlying == Paused {
			return StillPausedByAWS, nil
		}
		return RemovedManual, nil
	case Paused:
		if err := m.ReverseAndWait(ctx, budget, s.ActionID); err != nil {
			return "", err
		}
		return TurnedOff, m.record(ctx, role, by, reason)
	case Busy, Failed, NoAction:
		return "", fmt.Errorf("pause action is %s; check it in the AWS Budgets console", s.Text(m.now()))
	}
	return NotPaused, nil
}

// UnpauseAndRearm reverses an AWS Budgets pause and re-arms it at the budget's current threshold.
// apply uses it when a raised limit puts the trigger above spend.
func (m *Manager) UnpauseAndRearm(ctx context.Context, role, budget, actionID, by, reason string) error {
	if err := m.ReverseAndWait(ctx, budget, actionID); err != nil {
		return err
	}
	if err := m.Reset(ctx, budget, actionID); err != nil {
		return err
	}
	return m.record(ctx, role, by, reason)
}

func (m *Manager) record(ctx context.Context, role, by, reason string) error {
	if reason == "" {
		reason = "-"
	}
	_, err := m.IAM.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(role), Tags: []itypes.Tag{
		{Key: aws.String(TagUnpausedBy), Value: aws.String(TagValue(by))},
		{Key: aws.String(TagUnpausedAt), Value: aws.String(m.now().UTC().Format(time.RFC3339))},
		{Key: aws.String(TagUnpauseReason), Value: aws.String(TagValue(reason))},
	}})
	return err
}

func (m *Manager) actionStatus(ctx context.Context, budget, id string) (btypes.ActionStatus, error) {
	r, err := m.Budgets.DescribeBudgetAction(ctx, &budgets.DescribeBudgetActionInput{
		AccountId: aws.String(m.Account), BudgetName: aws.String(budget), ActionId: aws.String(id)})
	if err != nil {
		return "", err
	}
	return r.Action.Status, nil
}

func (m *Manager) execute(ctx context.Context, budget, id string, t btypes.ExecutionType) error {
	_, err := m.Budgets.ExecuteBudgetAction(ctx, &budgets.ExecuteBudgetActionInput{AccountId: aws.String(m.Account),
		BudgetName: aws.String(budget), ActionId: aws.String(id), ExecutionType: t})
	return err
}

func (m *Manager) waitWhile(ctx context.Context, budget, id string, busy btypes.ActionStatus) (btypes.ActionStatus, error) {
	poll, wait := m.Poll, m.Wait
	if poll == 0 {
		poll = time.Second
	}
	if wait == 0 {
		wait = 60 * time.Second
	}
	deadline := m.now().Add(wait)
	for {
		st, err := m.actionStatus(ctx, budget, id)
		if err != nil || st != busy {
			return st, err
		}
		if m.now().After(deadline) {
			return st, fmt.Errorf("budget action of %s still %s after %s", budget, st, wait)
		}
		m.sleep(poll)
	}
}

// ReverseAndWait reverses an executed action and waits for REVERSE_SUCCESS.
func (m *Manager) ReverseAndWait(ctx context.Context, budget, id string) error {
	if err := m.execute(ctx, budget, id, btypes.ExecutionTypeReverseBudgetAction); err != nil {
		return fmt.Errorf("reversing the pause of %s: %w", budget, err)
	}
	st, err := m.waitWhile(ctx, budget, id, btypes.ActionStatusReverseInProgress)
	if err != nil {
		return err
	}
	if st != btypes.ActionStatusReverseSuccess {
		return fmt.Errorf("reversing the pause of %s ended in %s", budget, st)
	}
	return nil
}

// Reset re-arms a reversed action (REVERSE_SUCCESS → STANDBY).
func (m *Manager) Reset(ctx context.Context, budget, id string) error {
	if err := m.execute(ctx, budget, id, btypes.ExecutionTypeResetBudgetAction); err != nil {
		return fmt.Errorf("re-arming the pause of %s: %w", budget, err)
	}
	st, err := m.waitWhile(ctx, budget, id, btypes.ActionStatusResetInProgress)
	if err != nil {
		return err
	}
	if st != btypes.ActionStatusStandby && st != btypes.ActionStatusPending {
		return fmt.Errorf("re-arming the pause of %s ended in %s", budget, st)
	}
	return nil
}

func isNoSuchEntity(err error) bool {
	var nse *itypes.NoSuchEntityException
	return errors.As(err, &nse)
}
