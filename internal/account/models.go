package account

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	rtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/SUSE/high-impact-ai-initiative/internal/awsx"
	"github.com/SUSE/high-impact-ai-initiative/internal/plan"
	"github.com/SUSE/high-impact-ai-initiative/internal/policy"
)

var geoPrefixes = []string{"us.", "eu.", "apac.", "global.", "jp.", "au.", "ca.", "us-gov."}

// FoundationModelID strips the cross-region inference profile prefix (us., eu., ...).
func FoundationModelID(model string) string {
	for _, p := range geoPrefixes {
		if m, ok := strings.CutPrefix(model, p); ok {
			return m
		}
	}
	return model
}

// ModelAvailable reports whether the account can call a model, with the reason when it can't.
func ModelAvailable(ctx context.Context, c *bedrock.Client, model string) (bool, string, error) {
	r, err := c.GetFoundationModelAvailability(ctx, &bedrock.GetFoundationModelAvailabilityInput{
		ModelId: aws.String(FoundationModelID(model))})
	if err != nil {
		return false, "", err
	}
	switch {
	case r.RegionAvailability != btypes.RegionAvailabilityAvailable:
		return false, "not available in this region", nil
	case r.AuthorizationStatus != btypes.AuthorizationStatusAuthorized:
		return false, "not authorized (model access use-case form)", nil
	case r.AgreementAvailability == nil || r.AgreementAvailability.Status != btypes.AgreementStatusAvailable:
		return false, "no agreement yet (first call accepts it)", nil
	case r.EntitlementAvailability != btypes.EntitlementAvailabilityAvailable:
		return false, "not enabled yet (first call enables it)", nil
	}
	return true, "", nil
}

// TestCall makes the smallest possible model call.
func TestCall(ctx context.Context, c *bedrockruntime.Client, model string) error {
	_, err := c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model),
		Messages: []rtypes.Message{{Role: rtypes.ConversationRoleUser,
			Content: []rtypes.ContentBlock{&rtypes.ContentBlockMemberText{Value: "Reply with OK."}}}},
		InferenceConfig: &rtypes.InferenceConfiguration{MaxTokens: aws.Int32(1)}})
	return err
}

func (e *Env) models(ctx context.Context) ([]*plan.Item, error) {
	var out []*plan.Item
	for _, m := range e.Cfg.Models {
		it := e.item("1.5", "model "+m)
		ok, why, err := ModelAvailable(ctx, e.C.Bedrock, m)
		switch {
		case err != nil && awsx.IsNotFound(err):
			it.Op, it.Status = plan.Conflict, "unknown model in "+e.Cfg.Region
		case err != nil:
			it.Op, it.Status = plan.Info, "can't check ("+awsx.ErrorCode(err)+"); doctor makes a test call"
		case !ok:
			it.Op, it.Details = plan.Create, []string{"first call"}
			it.Notes = []string{why}
			model := m
			it.Apply = func(ctx context.Context) error {
				if err := TestCall(ctx, e.C.Runtime, model); err != nil {
					return fmt.Errorf("first call to %s: %w", model, err)
				}
				return nil
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func (e *Env) costTag(ctx context.Context) ([]*plan.Item, error) {
	it := e.item("1.6", "cost allocation tag "+policy.CostAllocationOwnerTag)
	r, err := e.C.CE.ListCostAllocationTags(ctx, &costexplorer.ListCostAllocationTagsInput{
		TagKeys: []string{policy.CostAllocationOwnerTag}})
	if err != nil {
		if awsx.IsAccessDenied(err) || strings.Contains(strings.ToLower(err.Error()), "management account") ||
			strings.Contains(strings.ToLower(err.Error()), "payer") || strings.Contains(strings.ToLower(err.Error()), "linked account") {
			it.Op, it.Status = plan.Handoff, "needs org admin: activate it in the management account (Billing → Cost allocation tags)"
			return one(it), nil
		}
		return nil, fmt.Errorf("reading cost allocation tags: %w", err)
	}
	if len(r.CostAllocationTags) == 0 {
		it.Op, it.Status = plan.Info, "waiting for first use (appears up to 24 h after the first personal-role call)"
		return one(it), nil
	}
	if r.CostAllocationTags[0].Status != cetypes.CostAllocationTagStatusActive {
		it.Op, it.Details = plan.Update, []string{"activate"}
		it.Apply = func(ctx context.Context) error {
			out, err := e.C.CE.UpdateCostAllocationTagsStatus(ctx, &costexplorer.UpdateCostAllocationTagsStatusInput{
				CostAllocationTagsStatus: []cetypes.CostAllocationTagStatusEntry{{TagKey: aws.String(policy.CostAllocationOwnerTag),
					Status: cetypes.CostAllocationTagStatusActive}}})
			if err != nil {
				return fmt.Errorf("activating %s: %w", policy.CostAllocationOwnerTag, err)
			}
			if len(out.Errors) > 0 {
				return fmt.Errorf("activating %s: %s", policy.CostAllocationOwnerTag, aws.ToString(out.Errors[0].Message))
			}
			return nil
		}
	}
	return one(it), nil
}
