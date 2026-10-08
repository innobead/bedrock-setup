// Package account checks and makes the one-time account setup (Steps 1.1–1.7).
package account

import (
	"context"
	"fmt"
	"io"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/config"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

// Env is what every step needs.
type Env struct {
	C   *awsx.Clients
	Cfg *config.Config
	// Out receives progress and the Step 1.7 instructions during apply.
	Out io.Writer
	// Dir is where apply writes bedrock-block-policy.json.
	Dir string
}

func (e *Env) logf(format string, a ...any) {
	if e.Out != nil {
		_, _ = fmt.Fprintf(e.Out, format+"\n", a...)
	}
}

func (e *Env) item(step, target string) *plan.Item {
	return &plan.Item{Section: "account", Step: step, Target: target, Op: plan.OK, Status: "ok"}
}

// owned decides what to do with an existing resource from its bedrock-admin:id. It returns true
// when this config manages it; otherwise it marks the item.
func (e *Env) owned(it *plan.Item, id string) bool {
	switch id {
	case e.Cfg.ID:
		return true
	case "":
		it.Op = plan.Conflict
		it.Status = "exists without a " + policy.IDTag + " tag (made outside bedrock-admin); remove it, then apply"
	default:
		it.Op = plan.Info
		it.Status = "ok (managed by " + id + ")"
	}
	return false
}

func (e *Env) tags() map[string]string { return map[string]string{policy.IDTag: e.Cfg.ID} }

// Plan observes every step and returns its items, each with an Apply func when it's a change.
func (e *Env) Plan(ctx context.Context) ([]*plan.Item, error) {
	var out []*plan.Item
	steps := []func(context.Context) ([]*plan.Item, error){
		e.costExport, e.denyPolicy, e.budgetActionsRole, e.monthlyLambda, e.models, e.costTag, e.blockDirectCalls,
	}
	for _, s := range steps {
		items, err := s(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func one(it *plan.Item) []*plan.Item { return []*plan.Item{it} }
