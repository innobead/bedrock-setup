#!/bin/bash
# Show the PoC budget action status and whether bedrock-deny is attached to the personal role.
aws budgets describe-budget-action --account-id 111122223333 --budget-name poc-bedrock-achen \
  --action-id ACTION_ID --query 'Action.Status' --output text
aws iam list-attached-role-policies --role-name bedrock-user-achen --query 'AttachedPolicies[].PolicyName' --output text
aws budgets describe-budget-action-histories --account-id 111122223333 --budget-name poc-bedrock-achen \
  --action-id ACTION_ID \
  --query 'ActionHistories[].[Timestamp,Status,EventType]' --output text
