package users

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/account"
	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/iamrole"
	"github.com/SUSE/high-impact-ai-initiative/internal/pause"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// MaxSession is the personal role session length.
const MaxSession = 3600

// Env is what planning users needs.
type Env struct {
	C     *awsx.Clients
	Cfg   *config.Config
	Pause *pause.Manager
	Out   io.Writer
	Now   func() time.Time
	// ProfileRegion is the admin profile's region; setup commands add --region when it differs.
	ProfileRegion string
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// NewPause builds the pause manager for these clients.
func NewPause(c *awsx.Clients) *pause.Manager {
	return &pause.Manager{Budgets: c.Budgets, IAM: c.IAM, Account: c.Account, Poll: 3 * time.Second, Wait: 5 * time.Minute}
}

// RoleSpec is the personal role of a user.
func RoleSpec(cfg *config.Config, account string, u config.Resolved) iamrole.Spec {
	return iamrole.Spec{Name: u.RoleName, Path: policy.UsersPath,
		Description: "Personal Bedrock role of " + u.Email,
		Trust:       policy.UserTrust(account, u.Email), InlineName: policy.InvokePolicyName,
		Inline: policy.UserInvoke(account), MaxSession: MaxSession,
		Tags: map[string]string{policy.OwnerTag: u.Email, policy.ProductTag: cfg.Product, policy.IDTag: cfg.ID}}
}

// SetupCommand is the command to send the user.
func SetupCommand(cfg *config.Config, account, profileRegion string, u config.Resolved) string {
	cmd := "bedrock setup"
	if profileRegion != "" && cfg.Region != profileRegion {
		cmd += " --region " + cfg.Region
	}
	if u.CustomName {
		cmd += " --role-arn " + policy.UserRoleArn(account, u.RoleName)
	}
	return cmd
}

func budgetArn(account, name string) string {
	return fmt.Sprintf("arn:aws:budgets::%s:budget/%s", account, name)
}

// BudgetID reads a budget's bedrock-admin:id tag.
func BudgetID(ctx context.Context, c *budgets.Client, account, name string) (string, error) {
	r, err := c.ListTagsForResource(ctx, &budgets.ListTagsForResourceInput{ResourceARN: aws.String(budgetArn(account, name))})
	if err != nil {
		return "", err
	}
	for _, t := range r.ResourceTags {
		if aws.ToString(t.Key) == policy.IDTag {
			return aws.ToString(t.Value), nil
		}
	}
	return "", nil
}

func amount(s *btypes.Spend) float64 {
	if s == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(aws.ToString(s.Amount), 64)
	return v
}

// Spend returns the budget's actual and forecast spend this month.
func Spend(b *btypes.Budget) (actual, forecast float64) {
	if b == nil || b.CalculatedSpend == nil {
		return 0, 0
	}
	return amount(b.CalculatedSpend.ActualSpend), amount(b.CalculatedSpend.ForecastedSpend)
}

func filterOK(b *btypes.Budget, email string) bool {
	f := b.FilterExpression
	return f != nil && f.Tags != nil && aws.ToString(f.Tags.Key) == policy.CostAllocationOwnerTag &&
		slices.Equal(f.Tags.Values, []string{email})
}

func isAlert(n btypes.Notification) bool {
	return n.NotificationType == btypes.NotificationTypeActual && n.ComparisonOperator == btypes.ComparisonOperatorGreaterThan &&
		// AWS omits ThresholdType when it is the default, PERCENTAGE.
		(n.ThresholdType == btypes.ThresholdTypePercentage || n.ThresholdType == "")
}

func subscribers(subs []btypes.Subscriber) []string {
	var out []string
	for _, s := range subs {
		out = append(out, aws.ToString(s.Address))
	}
	sort.Strings(out)
	return out
}

// Observe reads one user's state.
func (e *Env) Observe(ctx context.Context, u config.Resolved) (Observed, error) {
	var o Observed
	acct := e.C.Account
	st, err := iamrole.Observe(ctx, e.C.IAM, RoleSpec(e.Cfg, acct, u))
	if err != nil {
		return o, err
	}
	o.RoleExists, o.RoleID, o.RoleDrift = st.Exists, st.ID(), st.Drift
	b, err := e.C.Budgets.DescribeBudget(ctx, &budgets.DescribeBudgetInput{AccountId: aws.String(acct), BudgetName: aws.String(u.BudgetName)})
	if err != nil {
		var nf *btypes.NotFoundException
		if errors.As(err, &nf) {
			return o, nil
		}
		return o, fmt.Errorf("reading budget %s: %w", u.BudgetName, err)
	}
	o.BudgetExists = true
	if o.BudgetID, err = BudgetID(ctx, e.C.Budgets, acct, u.BudgetName); err != nil {
		return o, fmt.Errorf("reading tags of budget %s: %w", u.BudgetName, err)
	}
	o.Limit = amount(b.Budget.BudgetLimit)
	o.FilterOK = filterOK(b.Budget, u.Email)
	o.Spend, o.Forecast = Spend(b.Budget)
	o.Alerts = map[int][]string{}
	ns, err := e.C.Budgets.DescribeNotificationsForBudget(ctx, &budgets.DescribeNotificationsForBudgetInput{
		AccountId: aws.String(acct), BudgetName: aws.String(u.BudgetName)})
	if err != nil {
		return o, fmt.Errorf("reading alerts of %s: %w", u.BudgetName, err)
	}
	for _, n := range ns.Notifications {
		if !isAlert(n) || n.Threshold != float64(int(n.Threshold)) {
			o.ExtraNotified++
			continue
		}
		s, err := e.C.Budgets.DescribeSubscribersForNotification(ctx, &budgets.DescribeSubscribersForNotificationInput{
			AccountId: aws.String(acct), BudgetName: aws.String(u.BudgetName), Notification: &n})
		if err != nil {
			return o, err
		}
		o.Alerts[int(n.Threshold)] = subscribers(s.Subscribers)
	}
	a, err := e.Pause.Action(ctx, u.BudgetName)
	if err != nil {
		return o, fmt.Errorf("reading the pause action of %s: %w", u.BudgetName, err)
	}
	if a != nil {
		o.ActionExists, o.ActionID = true, aws.ToString(a.ActionId)
		o.ActionShapeOK = actionShapeOK(*a, acct, u.RoleName)
		if a.ActionThreshold != nil {
			o.ActionThreshold = int(a.ActionThreshold.ActionThresholdValue)
		}
		o.ActionSubscribers = subscribers(a.Subscribers)
	}
	if o.RoleExists {
		if o.Pause, err = e.Pause.Status(ctx, u.RoleName, u.BudgetName); err != nil {
			return o, err
		}
	}
	return o, nil
}

func actionShapeOK(a btypes.Action, acct, role string) bool {
	d := a.Definition.IamActionDefinition
	return a.NotificationType == btypes.NotificationTypeActual && a.ApprovalModel == btypes.ApprovalModelAuto &&
		a.ActionThreshold != nil && a.ActionThreshold.ActionThresholdType == btypes.ThresholdTypePercentage &&
		aws.ToString(a.ExecutionRoleArn) == policy.BudgetActionsRoleArn(acct) &&
		slices.Equal(d.Roles, []string{role}) && len(d.Users) == 0 && len(d.Groups) == 0
}

// Plan returns one item per configured user, then one per managed user to remove and per
// unmanaged role. noDelete turns removals into notes.
func (e *Env) Plan(ctx context.Context, noDelete bool) ([]*plan.Item, error) {
	var out []*plan.Item
	want := map[string]bool{}
	for _, u := range e.Cfg.Resolve() {
		want[u.RoleName] = true
		o, err := e.Observe(ctx, u)
		if err != nil {
			return nil, err
		}
		c := Diff(u, o, e.Cfg.ID, e.now())
		it := &plan.Item{Section: "users", Target: u.Email, Email: u.Email, Op: c.Op, Details: c.Details, Notes: c.Notes,
			NeedsYes: c.NeedsYes, SetupCommand: SetupCommand(e.Cfg, e.C.Account, e.ProfileRegion, u)}
		if o.RoleExists {
			it.Pause = o.Pause.Text(e.now())
		}
		if c.Op == plan.OK {
			it.Status = "no changes"
		}
		if c.Op == plan.Create || c.Op == plan.Update {
			u, o, c := u, o, c
			it.Apply = func(ctx context.Context) error { return e.apply(ctx, u, o, c) }
		}
		out = append(out, it)
	}
	roles, err := account.UserRoles(ctx, e.C.IAM)
	if err != nil {
		return nil, fmt.Errorf("listing personal roles: %w", err)
	}
	for _, role := range roles {
		if want[role] {
			continue
		}
		tags, err := e.Pause.RoleTags(ctx, role)
		if err != nil {
			return nil, err
		}
		who := tags[policy.OwnerTag]
		if who == "" {
			who = role
		}
		it := &plan.Item{Section: "users", Target: who, Email: tags[policy.OwnerTag]}
		switch id := tags[policy.IDTag]; id {
		case e.Cfg.ID:
			it.Op, it.Details = plan.Delete, []string{"offboard"}
			if noDelete {
				it.Op, it.Details, it.Status = plan.Info, nil, "offboard skipped (--no-delete)"
			} else {
				it.NeedsYes = true
				name := strings.TrimPrefix(role, "bedrock-user-")
				it.Apply = func(ctx context.Context) error { return e.remove(ctx, role, config.BudgetName(name)) }
			}
		case "":
			it.Op, it.Status = plan.Info, "not managed (role "+role+" has no "+policy.IDTag+" tag)"
		default:
			it.Op, it.Status = plan.Info, "not managed (id "+id+")"
		}
		out = append(out, it)
	}
	return out, nil
}

func (e *Env) wantBudget(u config.Resolved) *btypes.Budget {
	return &btypes.Budget{BudgetName: aws.String(u.BudgetName), BudgetType: btypes.BudgetTypeCost, TimeUnit: btypes.TimeUnitMonthly,
		BudgetLimit: &btypes.Spend{Amount: aws.String(strconv.FormatFloat(u.LimitUSD, 'f', 2, 64)), Unit: aws.String("USD")},
		FilterExpression: &btypes.Expression{Tags: &btypes.TagValues{Key: aws.String(policy.CostAllocationOwnerTag),
			Values: []string{u.Email}, MatchOptions: []btypes.MatchOption{btypes.MatchOptionEquals}}},
		Metrics: []btypes.Metric{btypes.MetricUnblendedCost}}
}

func alert(p int) *btypes.Notification {
	return &btypes.Notification{NotificationType: btypes.NotificationTypeActual, ComparisonOperator: btypes.ComparisonOperatorGreaterThan,
		Threshold: float64(p), ThresholdType: btypes.ThresholdTypePercentage}
}

func email(addr string) []btypes.Subscriber {
	return []btypes.Subscriber{{SubscriptionType: btypes.SubscriptionTypeEmail, Address: aws.String(addr)}}
}

func retryable(err error) bool {
	// A new role (or the budget actions role) can take a few seconds to be usable.
	return strings.Contains(err.Error(), "role") || awsx.IsAccessDenied(err)
}

func (e *Env) apply(ctx context.Context, u config.Resolved, o Observed, c Change) error {
	acct := aws.String(e.C.Account)
	bn := aws.String(u.BudgetName)
	spec := RoleSpec(e.Cfg, e.C.Account, u)
	if c.CreateRole {
		if err := iamrole.Create(ctx, e.C.IAM, spec); err != nil {
			return err
		}
	} else if c.FixRole {
		st, err := iamrole.Observe(ctx, e.C.IAM, spec)
		if err != nil {
			return err
		}
		if err := iamrole.Fix(ctx, e.C.IAM, spec, st); err != nil {
			return err
		}
	}
	if c.CreateBudget {
		var ns []btypes.NotificationWithSubscribers
		for _, p := range u.AlertAtPercent {
			ns = append(ns, btypes.NotificationWithSubscribers{Notification: alert(p), Subscribers: email(u.Notify)})
		}
		if _, err := e.C.Budgets.CreateBudget(ctx, &budgets.CreateBudgetInput{AccountId: acct, Budget: e.wantBudget(u),
			NotificationsWithSubscribers: ns,
			ResourceTags:                 []btypes.ResourceTag{{Key: aws.String(policy.IDTag), Value: aws.String(e.Cfg.ID)}}}); err != nil {
			return fmt.Errorf("creating budget %s: %w", u.BudgetName, err)
		}
	}
	if c.UpdateBudget {
		if _, err := e.C.Budgets.UpdateBudget(ctx, &budgets.UpdateBudgetInput{AccountId: acct, NewBudget: e.wantBudget(u)}); err != nil {
			return fmt.Errorf("updating budget %s: %w", u.BudgetName, err)
		}
	}
	for _, p := range c.RemoveAlerts {
		if _, err := e.C.Budgets.DeleteNotification(ctx, &budgets.DeleteNotificationInput{AccountId: acct, BudgetName: bn, Notification: alert(p)}); err != nil {
			return fmt.Errorf("removing the %d%% alert of %s: %w", p, u.BudgetName, err)
		}
	}
	for _, p := range c.AddAlerts {
		if _, err := e.C.Budgets.CreateNotification(ctx, &budgets.CreateNotificationInput{AccountId: acct, BudgetName: bn,
			Notification: alert(p), Subscribers: email(u.Notify)}); err != nil {
			return fmt.Errorf("adding the %d%% alert of %s: %w", p, u.BudgetName, err)
		}
	}
	for _, p := range c.ResubAlert {
		for _, old := range o.Alerts[p] {
			if _, err := e.C.Budgets.DeleteSubscriber(ctx, &budgets.DeleteSubscriberInput{AccountId: acct, BudgetName: bn,
				Notification: alert(p), Subscriber: &email(old)[0]}); err != nil {
				return err
			}
		}
		if _, err := e.C.Budgets.CreateSubscriber(ctx, &budgets.CreateSubscriberInput{AccountId: acct, BudgetName: bn,
			Notification: alert(p), Subscriber: &email(u.Notify)[0]}); err != nil {
			return err
		}
	}
	threshold := &btypes.ActionThreshold{ActionThresholdValue: float64(u.PauseAtPercent), ActionThresholdType: btypes.ThresholdTypePercentage}
	if c.RecreateAction {
		if _, err := e.C.Budgets.DeleteBudgetAction(ctx, &budgets.DeleteBudgetActionInput{AccountId: acct, BudgetName: bn,
			ActionId: aws.String(o.ActionID)}); err != nil {
			return fmt.Errorf("removing the pause action of %s: %w", u.BudgetName, err)
		}
	}
	if c.CreateAction || c.RecreateAction {
		in := &budgets.CreateBudgetActionInput{AccountId: acct, BudgetName: bn, NotificationType: btypes.NotificationTypeActual,
			ActionType: btypes.ActionTypeIam, ActionThreshold: threshold,
			Definition: &btypes.Definition{IamActionDefinition: &btypes.IamActionDefinition{
				PolicyArn: aws.String(policy.DenyPolicyArn(e.C.Account)), Roles: []string{u.RoleName}}},
			ExecutionRoleArn: aws.String(policy.BudgetActionsRoleArn(e.C.Account)), ApprovalModel: btypes.ApprovalModelAuto,
			Subscribers: email(u.Notify)}
		err := iamrole.Retry(ctx, 60*time.Second, retryable, func() error { _, err := e.C.Budgets.CreateBudgetAction(ctx, in); return err })
		if err != nil {
			return fmt.Errorf("creating the pause action of %s: %w", u.BudgetName, err)
		}
	}
	if c.UpdateAction {
		if _, err := e.C.Budgets.UpdateBudgetAction(ctx, &budgets.UpdateBudgetActionInput{AccountId: acct, BudgetName: bn,
			ActionId: aws.String(o.ActionID), ActionThreshold: threshold, Subscribers: email(u.Notify)}); err != nil {
			return fmt.Errorf("updating the pause action of %s: %w", u.BudgetName, err)
		}
	}
	if c.Rearm {
		if err := e.Pause.UnpauseAndRearm(ctx, u.RoleName, u.BudgetName, o.ActionID, "bedrock-admin apply ("+
			awsx.Identity(e.C.CallerArn)+")", c.RearmReason); err != nil {
			return err
		}
	}
	return nil
}

// remove offboards a user: budget (with its actions), then the role.
func (e *Env) remove(ctx context.Context, role, budget string) error {
	acct := aws.String(e.C.Account)
	r, err := e.C.Budgets.DescribeBudgetActionsForBudget(ctx, &budgets.DescribeBudgetActionsForBudgetInput{AccountId: acct, BudgetName: aws.String(budget)})
	var nf *btypes.NotFoundException
	switch {
	case errors.As(err, &nf):
	case err != nil:
		return err
	default:
		for _, a := range r.Actions {
			if _, err := e.C.Budgets.DeleteBudgetAction(ctx, &budgets.DeleteBudgetActionInput{AccountId: acct,
				BudgetName: aws.String(budget), ActionId: a.ActionId}); err != nil {
				return fmt.Errorf("removing the pause action of %s: %w", budget, err)
			}
		}
		if id, err := BudgetID(ctx, e.C.Budgets, e.C.Account, budget); err != nil {
			return err
		} else if id == e.Cfg.ID {
			if _, err := e.C.Budgets.DeleteBudget(ctx, &budgets.DeleteBudgetInput{AccountId: acct, BudgetName: aws.String(budget)}); err != nil {
				return fmt.Errorf("removing budget %s: %w", budget, err)
			}
		}
	}
	return iamrole.Delete(ctx, e.C.IAM, role)
}

// Find resolves an email or name to a configured user, or to a role managed by this config.
func (e *Env) Find(ctx context.Context, who string) (config.Resolved, error) {
	if u, ok := e.Cfg.FindUser(who); ok {
		return u, nil
	}
	return config.Resolved{}, fmt.Errorf("%s is not in %s", who, e.Cfg.Path)
}
