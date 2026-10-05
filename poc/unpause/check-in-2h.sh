#!/bin/bash
# Sleeps ~2 h, then reports: both test actions, deny attachments, bedrock-achen spend.
export AWS_PROFILE=default AWS_REGION=us-west-2; ACCT=111122223333
sleep ${DELAY:-7080}
date -u
st() { aws budgets describe-budget-action --account-id $ACCT --budget-name $1 --action-id $2 --query Action.Status --output text 2>&1; }
echo "achen: spend=$(aws budgets describe-budget --account-id $ACCT --budget-name bedrock-achen --query Budget.CalculatedSpend.ActualSpend.Amount --output text 2>&1)/6 action=$(st bedrock-achen ACTION_ID) attached=[$(aws iam list-attached-role-policies --role-name bedrock-user-achen --query 'AttachedPolicies[].PolicyName' --output text)]"
echo "unpause-test: action=$(st bedrock-unpause-test TEST_ACTION_ID) attached=[$(aws iam list-attached-role-policies --role-name bedrock-user-unpause-test --query 'AttachedPolicies[].PolicyName' --output text)]"
echo "--- achen history"; aws budgets describe-budget-action-histories --account-id $ACCT --budget-name bedrock-achen --action-id ACTION_ID --query 'ActionHistories[].[Timestamp,Status,EventType]' --output text
