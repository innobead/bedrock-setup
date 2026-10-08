package pause

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	itypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

const acct = "123456789012"

// fake is AWS Budgets plus IAM in memory. An action moves through *_IN_PROGRESS for one describe.
type fake struct {
	actions  map[string]*btypes.Action // by budget
	limits   map[string]string
	tags     map[string]map[string]string // by role
	deny     map[string]bool              // role → bedrock-deny attached
	pending  map[string]btypes.ActionStatus
	executed []string
}

func newFake() *fake {
	return &fake{actions: map[string]*btypes.Action{}, limits: map[string]string{}, tags: map[string]map[string]string{},
		deny: map[string]bool{}, pending: map[string]btypes.ActionStatus{}}
}

func (f *fake) user(name, email string, status btypes.ActionStatus) {
	b, r := "bedrock-"+name, "bedrock-user-"+name
	f.actions[b] = &btypes.Action{ActionId: aws.String("id-" + name), BudgetName: aws.String(b), ActionType: btypes.ActionTypeIam,
		Status: status, ActionThreshold: &btypes.ActionThreshold{ActionThresholdValue: 100, ActionThresholdType: btypes.ThresholdTypePercentage},
		Definition: &btypes.Definition{IamActionDefinition: &btypes.IamActionDefinition{PolicyArn: aws.String(policy.DenyPolicyArn(acct)), Roles: []string{r}}}}
	f.limits[b] = "50.0"
	f.tags[r] = map[string]string{policy.OwnerTag: email}
	f.deny[r] = status == btypes.ActionStatusExecutionSuccess
}

func (f *fake) DescribeBudgetAction(_ context.Context, in *budgets.DescribeBudgetActionInput, _ ...func(*budgets.Options)) (*budgets.DescribeBudgetActionOutput, error) {
	b := aws.ToString(in.BudgetName)
	a := f.actions[b]
	if p, ok := f.pending[b]; ok {
		delete(f.pending, b)
		a.Status = p
		return &budgets.DescribeBudgetActionOutput{Action: &btypes.Action{Status: progress(p)}}, nil
	}
	return &budgets.DescribeBudgetActionOutput{Action: a}, nil
}

func progress(final btypes.ActionStatus) btypes.ActionStatus {
	if final == btypes.ActionStatusReverseSuccess {
		return btypes.ActionStatusReverseInProgress
	}
	return btypes.ActionStatusResetInProgress
}

func (f *fake) ExecuteBudgetAction(_ context.Context, in *budgets.ExecuteBudgetActionInput, _ ...func(*budgets.Options)) (*budgets.ExecuteBudgetActionOutput, error) {
	b := aws.ToString(in.BudgetName)
	a := f.actions[b]
	f.executed = append(f.executed, b+":"+string(in.ExecutionType))
	role := a.Definition.IamActionDefinition.Roles[0]
	switch in.ExecutionType {
	case btypes.ExecutionTypeReverseBudgetAction:
		if a.Status != btypes.ActionStatusExecutionSuccess {
			return nil, errors.New("not executed")
		}
		f.deny[role] = false
		f.pending[b] = btypes.ActionStatusReverseSuccess
	case btypes.ExecutionTypeResetBudgetAction:
		if a.Status != btypes.ActionStatusReverseSuccess {
			return nil, errors.New("not reversed")
		}
		f.pending[b] = btypes.ActionStatusStandby
	}
	return &budgets.ExecuteBudgetActionOutput{}, nil
}

func (f *fake) DescribeBudgetActionsForBudget(_ context.Context, in *budgets.DescribeBudgetActionsForBudgetInput, _ ...func(*budgets.Options)) (*budgets.DescribeBudgetActionsForBudgetOutput, error) {
	a, ok := f.actions[aws.ToString(in.BudgetName)]
	if !ok {
		return nil, &btypes.NotFoundException{}
	}
	return &budgets.DescribeBudgetActionsForBudgetOutput{Actions: []btypes.Action{*a}}, nil
}

func (f *fake) DescribeBudgetActionsForAccount(context.Context, *budgets.DescribeBudgetActionsForAccountInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetActionsForAccountOutput, error) {
	out := &budgets.DescribeBudgetActionsForAccountOutput{}
	for _, a := range f.actions {
		out.Actions = append(out.Actions, *a)
	}
	return out, nil
}

func (f *fake) DescribeBudgetActionHistories(context.Context, *budgets.DescribeBudgetActionHistoriesInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetActionHistoriesOutput, error) {
	ts := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	return &budgets.DescribeBudgetActionHistoriesOutput{ActionHistories: []btypes.ActionHistory{
		{Status: btypes.ActionStatusExecutionSuccess, Timestamp: &ts}}}, nil
}

func (f *fake) DescribeBudgets(context.Context, *budgets.DescribeBudgetsInput, ...func(*budgets.Options)) (*budgets.DescribeBudgetsOutput, error) {
	out := &budgets.DescribeBudgetsOutput{}
	for b, l := range f.limits {
		out.Budgets = append(out.Budgets, btypes.Budget{BudgetName: aws.String(b), BudgetLimit: &btypes.Spend{Amount: aws.String(l), Unit: aws.String("USD")}})
	}
	return out, nil
}

func (f *fake) ListRoleTags(_ context.Context, in *iam.ListRoleTagsInput, _ ...func(*iam.Options)) (*iam.ListRoleTagsOutput, error) {
	t, ok := f.tags[aws.ToString(in.RoleName)]
	if !ok {
		return nil, &itypes.NoSuchEntityException{}
	}
	out := &iam.ListRoleTagsOutput{}
	for k, v := range t {
		out.Tags = append(out.Tags, itypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fake) TagRole(_ context.Context, in *iam.TagRoleInput, _ ...func(*iam.Options)) (*iam.TagRoleOutput, error) {
	for _, t := range in.Tags {
		f.tags[aws.ToString(in.RoleName)][aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return &iam.TagRoleOutput{}, nil
}

func (f *fake) UntagRole(_ context.Context, in *iam.UntagRoleInput, _ ...func(*iam.Options)) (*iam.UntagRoleOutput, error) {
	for _, k := range in.TagKeys {
		delete(f.tags[aws.ToString(in.RoleName)], k)
	}
	return &iam.UntagRoleOutput{}, nil
}

func (f *fake) AttachRolePolicy(_ context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	f.deny[aws.ToString(in.RoleName)] = true
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *fake) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.deny[aws.ToString(in.RoleName)] = false
	return &iam.DetachRolePolicyOutput{}, nil
}

var now = time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)

func manager(f *fake) *Manager {
	return &Manager{Budgets: f, IAM: f, Account: acct, Now: func() time.Time { return now }, Sleep: func(time.Duration) {}}
}

func TestPauseUnpauseByHand(t *testing.T) {
	f := newFake()
	f.user("erin", "erin@acme.com", btypes.ActionStatusStandby)
	m := manager(f)
	ctx := context.Background()
	if err := m.Pause(ctx, "bedrock-user-erin", "admin@acme.com", "key leak!"); err != nil {
		t.Fatal(err)
	}
	s, _ := m.Status(ctx, "bedrock-user-erin", "bedrock-erin")
	if s.State != PausedByHand || s.Underlying != Armed || !f.deny["bedrock-user-erin"] || s.Reason != "key leak" {
		t.Fatalf("got %+v", s)
	}
	if got := s.Text(now); got != "PAUSED by hand (Nov 1: key leak)" {
		t.Fatalf("text %q", got)
	}
	r, err := m.Unpause(ctx, "bedrock-user-erin", "bedrock-erin", "admin@acme.com", "")
	if err != nil || r != RemovedManual || f.deny["bedrock-user-erin"] {
		t.Fatalf("got %s %v deny=%v", r, err, f.deny["bedrock-user-erin"])
	}
	if f.tags["bedrock-user-erin"][TagUnpausedBy] != "admin@acme.com" {
		t.Fatal("unpause not recorded")
	}
	if r, _ := m.Unpause(ctx, "bedrock-user-erin", "bedrock-erin", "x", ""); r != NotPaused {
		t.Fatalf("second unpause: %s", r)
	}
}

func TestUnpauseOverLimitTurnsOff(t *testing.T) {
	f := newFake()
	f.user("dave", "dave@acme.com", btypes.ActionStatusExecutionSuccess)
	m := manager(f)
	r, err := m.Unpause(context.Background(), "bedrock-user-dave", "bedrock-dave", "admin", "raised")
	if err != nil || r != TurnedOff || f.actions["bedrock-dave"].Status != btypes.ActionStatusReverseSuccess || f.deny["bedrock-user-dave"] {
		t.Fatalf("got %s %v %+v", r, err, f.actions["bedrock-dave"].Status)
	}
}

func TestUnpauseHandOverAWSPauseKeepsDeny(t *testing.T) {
	f := newFake()
	f.user("dave", "dave@acme.com", btypes.ActionStatusExecutionSuccess)
	m := manager(f)
	ctx := context.Background()
	_ = m.Pause(ctx, "bedrock-user-dave", "admin", "x")
	r, err := m.Unpause(ctx, "bedrock-user-dave", "bedrock-dave", "admin", "")
	if err != nil || r != StillPausedByAWS || !f.deny["bedrock-user-dave"] {
		t.Fatalf("got %s %v", r, err)
	}
}

func TestMonthly(t *testing.T) {
	f := newFake()
	f.user("alice", "alice@acme.com", btypes.ActionStatusStandby)
	f.user("carol", "carol@acme.com", btypes.ActionStatusReverseSuccess)
	f.user("dave", "dave@acme.com", btypes.ActionStatusExecutionSuccess)
	f.user("erin", "erin@acme.com", btypes.ActionStatusExecutionSuccess)
	m := manager(f)
	ctx := context.Background()
	_ = m.Pause(ctx, "bedrock-user-erin", "admin", "key leak")
	var saved Snapshot
	var order []string
	res, err := m.Monthly(ctx, func(_ context.Context, s Snapshot) error {
		saved = s
		order = append(order, "save:"+itoa(len(f.executed)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if order[0] != "save:0" {
		t.Fatal("snapshot must be saved before re-arming")
	}
	if saved.Month != "2026-10" || len(saved.Users) != 4 {
		t.Fatalf("snapshot %+v", saved)
	}
	d, _ := saved.Find("dave@acme.com")
	if d.State != Paused || d.TriggerUSD != 50 || d.PausedAt.IsZero() {
		t.Fatalf("dave %+v", d)
	}
	e, _ := saved.Find("erin@acme.com")
	if e.State != PausedByHand || e.Reason != "key leak" {
		t.Fatalf("erin %+v", e)
	}
	for _, b := range []string{"bedrock-alice", "bedrock-carol", "bedrock-dave", "bedrock-erin"} {
		if s := f.actions[b].Status; s != btypes.ActionStatusStandby {
			t.Errorf("%s: %s", b, s)
		}
	}
	if f.deny["bedrock-user-dave"] || f.deny["bedrock-user-carol"] {
		t.Error("dave and carol should be unpaused")
	}
	if !f.deny["bedrock-user-erin"] {
		t.Error("erin's pause by hand must stay")
	}
	if res.Done["bedrock-alice"] != "unchanged (STANDBY)" {
		t.Errorf("alice: %s", res.Done["bedrock-alice"])
	}
}

func TestMonthlySaveFailureStillRearms(t *testing.T) {
	f := newFake()
	f.user("dave", "dave@acme.com", btypes.ActionStatusExecutionSuccess)
	_, err := manager(f).Monthly(context.Background(), func(context.Context, Snapshot) error { return errors.New("s3 down") })
	if err == nil || f.actions["bedrock-dave"].Status != btypes.ActionStatusStandby {
		t.Fatalf("err=%v status=%s", err, f.actions["bedrock-dave"].Status)
	}
}

func TestText(t *testing.T) {
	cases := map[State]string{Armed: "armed", Off: "off until Dec 1", NoAction: "no pause action"}
	for st, want := range cases {
		if got := (Status{State: st}).Text(now); got != want {
			t.Errorf("%s: %q", st, got)
		}
	}
	p := Status{State: Paused, PausedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
	if p.Text(now) != "PAUSED (Oct 6)" {
		t.Error(p.Text(now))
	}
}

func TestPreviousMonth(t *testing.T) {
	if got := PreviousMonth(time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC)); got != "2025-12" {
		t.Fatal(got)
	}
	if SnapshotKey("/bedrock-admin/snapshots/", "2025-12") != "bedrock-admin/snapshots/2025-12.json" {
		t.Fatal("key")
	}
}

func itoa(i int) string { return string(rune('0' + i)) }
