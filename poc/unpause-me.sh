#!/bin/bash
# Run in a normal terminal (not inside Claude Code) if bedrock-user-achen gets paused.
# Uses the Admin SSO profile to reverse the pause and re-arm the action.
export AWS_PROFILE=default AWS_REGION=us-west-2
B=bedrock-achen; A=ACTION_ID; ACCT=111122223333
aws budgets execute-budget-action --account-id $ACCT --budget-name $B --action-id $A --execution-type REVERSE_BUDGET_ACTION >/dev/null
sleep 3
echo "status: $(aws budgets describe-budget-action --account-id $ACCT --budget-name $B --action-id $A --query Action.Status --output text)"
echo "attached: [$(aws iam list-attached-role-policies --role-name bedrock-user-achen --query 'AttachedPolicies[].PolicyName' --output text)]"
