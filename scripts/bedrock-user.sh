#!/bin/bash
# Manage one engineer's Bedrock access: personal role, monthly budget and automatic pause.
#
#   bedrock-user.sh onboard    <name> <sso-email> <notify-email> <monthly-usd> <product> [--all-models]
#   bedrock-user.sh status     <name>
#   bedrock-user.sh set-limit  <name> <monthly-usd>
#   bedrock-user.sh set-models <name> <claude|all>
#   bedrock-user.sh unpause    <name>
#   bedrock-user.sh offboard   <name>
#
# New engineers can use Claude models only, unless onboarded with --all-models.
#
# Run with an admin profile in the Bedrock account, e.g. AWS_PROFILE=default.
# The one-time account setup (bedrock-deny policy, budget-actions role) must already exist.
set -euo pipefail

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
die() { echo "error: $*" >&2; exit 1; }

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
DENY_ARN="arn:aws:iam::${ACCOUNT_ID}:policy/bedrock/bedrock-deny"
EXEC_ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/bedrock/bedrock-budget-actions"

role_name() { echo "bedrock-user-$1"; }
budget_name() { echo "bedrock-$1"; }

check_name() {
  [[ "$1" =~ ^[a-z0-9][a-z0-9.-]{0,40}$ ]] || die "name must be lowercase letters, digits, '.' or '-': $1"
}
check_email() { [[ "$1" =~ ^[^@[:space:]]+@[^@[:space:]]+$ ]] || die "not an email: $1"; }
check_usd() { [[ "$1" =~ ^[0-9]+(\.[0-9]{1,2})?$ ]] || die "not a dollar amount: $1"; }

# The cost tag on the role; the budget filters on it.
owner_of() {
  aws iam list-role-tags --role-name "$(role_name "$1")" \
    --query "Tags[?Key=='owner'].Value | [0]" --output text
}

action_id_of() {
  aws budgets describe-budget-actions-for-budget --account-id "$ACCOUNT_ID" \
    --budget-name "$(budget_name "$1")" --query 'Actions[0].ActionId' --output text
}

# Permissions of the personal role. "claude" allows Anthropic Claude models only (regional models,
# global models with an empty region, and us./eu./global. inference profiles); "all" allows any model.
invoke_policy_json() { # <claude|all>
  local models profiles
  case $1 in
    claude) models="anthropic.claude-*"; profiles="*anthropic.claude-*" ;;
    all)    models="*";                  profiles="*" ;;
    *)      die "model scope must be 'claude' or 'all': $1" ;;
  esac
  cat <<EOF
{"Version":"2012-10-17","Statement":[
  {"Effect":"Allow","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream"],
   "Resource":["arn:aws:bedrock:*::foundation-model/${models}","arn:aws:bedrock:*:${ACCOUNT_ID}:inference-profile/${profiles}"]},
  {"Effect":"Allow","Action":["bedrock:ListFoundationModels","bedrock:GetFoundationModel",
     "bedrock:ListInferenceProfiles","bedrock:GetInferenceProfile"],"Resource":"*"}]}
EOF
}

model_scope_of() {
  if aws iam get-role-policy --role-name "$(role_name "$1")" --policy-name bedrock-invoke \
       --query PolicyDocument --output json | grep -q 'anthropic.claude-'; then
    echo "Claude models only"
  else
    echo "all models"
  fi
}

budget_json() { # <name> <limit> <owner>
  cat <<EOF
{"BudgetName":"$(budget_name "$1")","BudgetType":"COST","TimeUnit":"MONTHLY",
 "BudgetLimit":{"Amount":"$2","Unit":"USD"},
 "FilterExpression":{"Tags":{"Key":"iamPrincipal/owner","Values":["$3"],"MatchOptions":["EQUALS"]}},
 "Metrics":["UnblendedCost"]}
EOF
}

cmd_onboard() {
  local scope=claude
  if [ $# -eq 6 ] && [ "$6" = --all-models ]; then scope=all; set -- "$1" "$2" "$3" "$4" "$5"; fi
  [ $# -eq 5 ] || usage
  local name=$1 email=$2 notify=$3 limit=$4 product=$5
  check_name "$name"; check_email "$email"; check_email "$notify"; check_usd "$limit"
  local role; role=$(role_name "$name")
  # The cost tag is the SSO login email, so every person has exactly one name in cost reports.
  local owner=$email

  aws iam get-policy --policy-arn "$DENY_ARN" >/dev/null 2>&1 \
    || die "one-time account setup missing: $DENY_ARN (see bedrock-admin-guide.md, Part 1)"

  local tmp; tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  cat > "$tmp/trust.json" <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow",
  "Principal":{"AWS":"arn:aws:iam::${ACCOUNT_ID}:root"},"Action":"sts:AssumeRole",
  "Condition":{
    "ArnLike":{"aws:PrincipalArn":"arn:aws:iam::${ACCOUNT_ID}:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_*"},
    "StringLike":{"aws:userid":"*:${email}"},
    "StringEquals":{"sts:RoleSessionName":"${email}"}}}]}
EOF
  invoke_policy_json "$scope" > "$tmp/invoke.json"

  echo "Creating role $role ..."
  aws iam create-role --role-name "$role" --path /bedrock-users/ \
    --assume-role-policy-document "file://$tmp/trust.json" --max-session-duration 3600 \
    --tags Key=owner,Value="$owner" Key=product,Value="$product" >/dev/null
  aws iam put-role-policy --role-name "$role" --policy-name bedrock-invoke \
    --policy-document "file://$tmp/invoke.json"

  echo "Model access: $([ "$scope" = claude ] && echo "Claude models only" || echo "all models")"
  echo "Creating budget $(budget_name "$name") (\$$limit/month, alert at 80% to $notify) ..."
  aws budgets create-budget --account-id "$ACCOUNT_ID" --budget "$(budget_json "$name" "$limit" "$owner")" \
    --notifications-with-subscribers "[{\"Notification\":{\"NotificationType\":\"ACTUAL\",
      \"ComparisonOperator\":\"GREATER_THAN\",\"Threshold\":80,\"ThresholdType\":\"PERCENTAGE\"},
      \"Subscribers\":[{\"SubscriptionType\":\"EMAIL\",\"Address\":\"$notify\"}]}]"

  echo "Creating automatic pause at 100% ..."
  aws budgets create-budget-action --account-id "$ACCOUNT_ID" --budget-name "$(budget_name "$name")" \
    --notification-type ACTUAL --action-type APPLY_IAM_POLICY \
    --action-threshold ActionThresholdValue=100,ActionThresholdType=PERCENTAGE \
    --definition "{\"IamActionDefinition\":{\"PolicyArn\":\"$DENY_ARN\",\"Roles\":[\"$role\"]}}" \
    --execution-role-arn "$EXEC_ROLE_ARN" --approval-model AUTOMATIC \
    --subscribers SubscriptionType=EMAIL,Address="$notify" >/dev/null

  cat <<EOF

Done. Send this to the engineer, with the user guide:

  Account ID:        $ACCOUNT_ID
  role_arn:          arn:aws:iam::${ACCOUNT_ID}:role/bedrock-users/$role
  role_session_name: $email
EOF
}

cmd_status() {
  [ $# -eq 1 ] || usage
  local name=$1 b; b=$(budget_name "$name")
  aws budgets describe-budget --account-id "$ACCOUNT_ID" --budget-name "$b" \
    --query 'Budget.{limit:BudgetLimit.Amount,spent_this_month:CalculatedSpend.ActualSpend.Amount,owner:FilterExpression.Tags.Values[0]}' \
    --output table
  local status; status=$(aws budgets describe-budget-action --account-id "$ACCOUNT_ID" --budget-name "$b" \
    --action-id "$(action_id_of "$name")" --query Action.Status --output text)
  case $status in
    EXECUTION_SUCCESS) echo "Paused: yes (over budget). Run: $0 unpause $name" ;;
    STANDBY|PENDING)   echo "Paused: no" ;;
    REVERSE_SUCCESS)   echo "Paused: no, but the pause is disarmed. Run: $0 unpause $name" ;;
    *)                 echo "Pause status: $status" ;;
  esac
  echo "Model access: $(model_scope_of "$name")"
}

cmd_set_models() {
  [ $# -eq 2 ] || usage
  local name=$1 scope=$2 tmp
  tmp=$(mktemp); trap 'rm -f "$tmp"' RETURN
  invoke_policy_json "$scope" > "$tmp"
  aws iam put-role-policy --role-name "$(role_name "$name")" --policy-name bedrock-invoke \
    --policy-document "file://$tmp"
  echo "Model access for $name is now: $(model_scope_of "$name"). It takes effect in about 20 seconds."
}

cmd_set_limit() {
  [ $# -eq 2 ] || usage
  local name=$1 limit=$2; check_usd "$limit"
  aws budgets update-budget --account-id "$ACCOUNT_ID" --new-budget "$(budget_json "$name" "$limit" "$(owner_of "$name")")"
  echo "Limit for $name is now \$$limit/month."
}

cmd_unpause() {
  [ $# -eq 1 ] || usage
  local name=$1 b id status; b=$(budget_name "$name"); id=$(action_id_of "$name")
  status=$(aws budgets describe-budget-action --account-id "$ACCOUNT_ID" --budget-name "$b" \
    --action-id "$id" --query Action.Status --output text)
  if [ "$status" = STANDBY ] || [ "$status" = PENDING ]; then
    echo "$name is not paused."; return
  fi
  if [ "$status" = EXECUTION_SUCCESS ]; then
    aws budgets execute-budget-action --account-id "$ACCOUNT_ID" --budget-name "$b" --action-id "$id" \
      --execution-type REVERSE_BUDGET_ACTION >/dev/null
    for _ in $(seq 30); do
      status=$(aws budgets describe-budget-action --account-id "$ACCOUNT_ID" --budget-name "$b" \
        --action-id "$id" --query Action.Status --output text)
      [ "$status" = REVERSE_IN_PROGRESS ] || break
      sleep 1
    done
  fi
  # Re-arm the pause, otherwise it never fires again.
  if [ "$status" = REVERSE_SUCCESS ]; then
    aws budgets execute-budget-action --account-id "$ACCOUNT_ID" --budget-name "$b" --action-id "$id" \
      --execution-type RESET_BUDGET_ACTION >/dev/null
  fi
  echo "$name is unpaused. Access returns in about 20 seconds."
  echo "Note: if they are still over budget, they will be paused again within hours. Raise the limit with set-limit first."
}

cmd_offboard() {
  [ $# -eq 1 ] || usage
  local name=$1 role; role=$(role_name "$name")
  aws budgets delete-budget --account-id "$ACCOUNT_ID" --budget-name "$(budget_name "$name")" \
    || echo "(no budget found)"
  aws iam detach-role-policy --role-name "$role" --policy-arn "$DENY_ARN" 2>/dev/null || true
  aws iam delete-role-policy --role-name "$role" --policy-name bedrock-invoke 2>/dev/null || true
  aws iam delete-role --role-name "$role"
  echo "Removed $role and its budget."
}

[ $# -ge 1 ] || usage
cmd=$1; shift
case $cmd in
  onboard)   cmd_onboard "$@" ;;
  status)    cmd_status "$@" ;;
  set-limit) cmd_set_limit "$@" ;;
  set-models) cmd_set_models "$@" ;;
  unpause)   cmd_unpause "$@" ;;
  offboard)  cmd_offboard "$@" ;;
  *)         usage ;;
esac
