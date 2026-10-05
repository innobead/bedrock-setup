#!/bin/bash
# Polls every 20 min (max ~115 min). Exits early when the owner tag shows a new value,
# a new CUR file lands, or the test action changes from REVERSE_SUCCESS.
export AWS_PROFILE=default AWS_REGION=us-west-2
B=bedrock-unpause-test; A=TEST_ACTION_ID
for i in 1 2 3 4 5 6; do
  vals=$(aws ce get-tags --time-period Start=2026-09-01,End=2026-10-05 --tag-key owner --query Tags --output text 2>&1)
  st=$(aws budgets describe-budget-action --account-id 111122223333 --budget-name $B --action-id $A --query Action.Status --output text 2>&1)
  cur=$(aws s3 ls s3://my-billing-bucket/cur/my-export/data/BILLING_PERIOD=2026-10/ 2>&1 | sort | tail -1 | awk '{print $1" "$2}')
  echo "$(date -u +%m-%d\ %H:%M) owner=[$vals] action=$st cur=$cur"
  echo "$vals" | grep -qi 'achen\|achen' && { echo "TAG_FOUND"; exit 0; }
  [ "$st" != "REVERSE_SUCCESS" ] && { echo "ACTION_CHANGED"; exit 0; }
  [ "$cur" != "2026-10-01 23:26:37" ] && { echo "NEW_CUR"; exit 0; }
  [ $i -lt 6 ] && sleep 1200
done
echo "NO_CHANGE"
