#!/bin/bash
# Usage: wait-for-status.sh <status-to-leave>   e.g. STANDBY
# Polls the test action every 10 min (max ~115 min) and exits when its status differs.
export AWS_PROFILE=default AWS_REGION=us-west-2
from=${1:-STANDBY}
for i in $(seq 1 11); do
  s=$(aws budgets describe-budget-action --account-id 111122223333 --budget-name bedrock-unpause-test \
      --action-id TEST_ACTION_ID --query Action.Status --output text 2>&1)
  spend=$(aws budgets describe-budget --account-id 111122223333 --budget-name bedrock-unpause-test \
      --query Budget.CalculatedSpend.ActualSpend.Amount --output text 2>&1)
  echo "$(date -u +%H:%M) status=$s spend=$spend"
  [ "$s" != "$from" ] && exit 0
  sleep 600
done
echo "still $from after polling window"
