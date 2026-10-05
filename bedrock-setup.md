# Amazon Bedrock: Setup Guide

> **Status: DRAFT v0.2 (2026-10-03).** For managers/admins and engineers who set up Bedrock access.
> The background, design reasoning and test results are in
> [bedrock-guideline.md](bedrock-guideline.md).
> ✅ = tested · ⏳ = waiting on IT or a test, do not use yet

## What you get

- Every engineer uses Claude and other models through Amazon Bedrock with their **SSO login**.
- Every engineer has a **personal monthly budget**.
- When someone goes over budget, **only that person is paused**. Everyone else keeps working.

## Who does what

| Phase | Who | When | Time |
|---|---|---|---|
| [Phase 0: One-time account setup](#phase-0-one-time-account-setup) | Account admin + IT | Once per AWS account | ~30 min, plus up to 48 h of AWS waiting |
| [Phase 1: Onboard an engineer](#phase-1-onboard-an-engineer) | Manager / admin | Once per engineer | ~5 min |
| [Phase 2: Engineer setup](#phase-2-engineer-setup) | Engineer | Once per laptop | ~5 min |
| [Phase 3: Day-to-day operations](#phase-3-day-to-day-operations) | Manager / admin | As needed | — |

---

## Phase 0: One-time account setup

**Who:** an admin of the AWS account where Bedrock is used, plus IT for the steps that need the
management account.

Set this once in your shell. All commands below use it.

```bash
export ACCOUNT_ID=<aws-account-id>          # e.g. 111122223333
export AWS_PROFILE=<your-admin-sso-profile> # e.g. default
```

### Step 0.1 Create the cost export ✅

This gives per-person cost reports.

1. Open *Billing and Cost Management → Data Exports → Create*.
2. Choose **Standard data export (CUR 2.0)**.
3. Under **Additional export content**, tick **Include caller identity (IAM principal) allocation data**.
4. Choose Parquet, hourly, and an S3 bucket.

### Step 0.2 Create the shared "pause" policy ✅

One policy is shared by everyone. AWS Budgets attaches it to a person's role to pause them.

```bash
cat > bedrock-deny.json <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "PauseBedrockModelUsage",
    "Effect": "Deny",
    "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream",
               "bedrock:CreateModelInvocationJob", "bedrock:CallWithBearerToken"],
    "Resource": "*"
  }]
}
EOF
aws iam create-policy --policy-name bedrock-deny --path /bedrock/ \
  --policy-document file://bedrock-deny.json
```

### Step 0.3 Create the role that AWS Budgets uses ✅

This role may **only** attach or detach the pause policy, and **only** on personal Bedrock roles.

```bash
cat > budget-actions-trust.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "budgets.amazonaws.com" },
    "Action": "sts:AssumeRole",
    "Condition": {
      "StringEquals": { "aws:SourceAccount": "${ACCOUNT_ID}" },
      "ArnLike": { "aws:SourceArn": "arn:aws:budgets::${ACCOUNT_ID}:budget/*" }
    }
  }]
}
EOF
cat > budget-actions-permissions.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["iam:AttachRolePolicy", "iam:DetachRolePolicy"],
    "Resource": "arn:aws:iam::${ACCOUNT_ID}:role/bedrock-users/*",
    "Condition": { "ArnEquals": { "iam:PolicyARN": "arn:aws:iam::${ACCOUNT_ID}:policy/bedrock/bedrock-deny" } }
  }]
}
EOF
aws iam create-role --role-name bedrock-budget-actions --path /bedrock/ \
  --assume-role-policy-document file://budget-actions-trust.json
aws iam put-role-policy --role-name bedrock-budget-actions \
  --policy-name attach-bedrock-deny --policy-document file://budget-actions-permissions.json
```

### Step 0.4 Activate the cost allocation tags (IT, management account) ✅

Do this **after the first engineer has finished Phase 2 and made one Bedrock call**. Before that,
the tags don't appear in the list.

1. Wait up to 24 h after that first call.
2. In the **management account**, open *Billing → Cost Allocation Tags*.
3. Set the filter **Tag type = IAM principal**. ⚠️ Not *Resource*: the same tag names may also be
   listed as resource tags, and those don't track Bedrock usage.
4. Activate `owner` and `product`. Activation can take up to another 24 h.

This is needed only once. People onboarded later are covered automatically.

**Check:** IT's tag list (filtered to *Type = IAM principal*) shows `owner` and `product` as
**Active**. About a day later, Cost Explorer → *Group by Tag: **iamPrincipal/owner*** shows the
engineers' emails. (Plain `owner` is the resource tag and does not include Bedrock spend.)

### Step 0.5 Close the side doors (IT) ⏳

Budgets only work if people call Bedrock **through their personal role**. Agree with IT on:

- denying `bedrock:InvokeModel*` in the Admin and PowerUser permission sets,
- no IAM users and no long-term Bedrock API keys for model access.

The exact policy is still being drafted.

### Step 0.6 Turn on the monthly auto-unpause ✅

AWS does **not** lift a pause when a new month starts. This scheduled job does it: at 06:00 UTC on
the 1st it unpauses everyone and re-arms their pause for the new month. One job covers all
engineers. Its code is [`poc/unpause/lambda_function.py`](poc/unpause/lambda_function.py).

```bash
cd poc/unpause
cat > lambda-trust.json <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},
  "Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"${ACCOUNT_ID}"}}}]}
EOF
cat > lambda-permissions.json <<EOF
{"Version":"2012-10-17","Statement":[
  {"Effect":"Allow","Action":["budgets:DescribeBudgetActionsForAccount","budgets:DescribeBudgetAction",
     "budgets:ExecuteBudgetAction"],"Resource":"*"},
  {"Effect":"Allow","Action":["logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],
   "Resource":"arn:aws:logs:us-west-2:${ACCOUNT_ID}:log-group:/aws/lambda/bedrock-monthly-unpause*"}]}
EOF
cat > scheduler-trust.json <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"scheduler.amazonaws.com"},
  "Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"${ACCOUNT_ID}"}}}]}
EOF
cat > scheduler-permissions.json <<EOF
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"lambda:InvokeFunction",
  "Resource":"arn:aws:lambda:us-west-2:${ACCOUNT_ID}:function:bedrock-monthly-unpause"}]}
EOF

aws iam create-role --role-name bedrock-monthly-unpause --path /bedrock/ \
  --assume-role-policy-document file://lambda-trust.json
aws iam put-role-policy --role-name bedrock-monthly-unpause \
  --policy-name reverse-bedrock-budget-actions --policy-document file://lambda-permissions.json
aws iam create-role --role-name bedrock-monthly-unpause-scheduler --path /bedrock/ \
  --assume-role-policy-document file://scheduler-trust.json
aws iam put-role-policy --role-name bedrock-monthly-unpause-scheduler \
  --policy-name invoke-unpause-lambda --policy-document file://scheduler-permissions.json
sleep 10   # let IAM propagate the new role

zip -q function.zip lambda_function.py
aws lambda create-function --function-name bedrock-monthly-unpause --runtime python3.13 \
  --handler lambda_function.handler --timeout 60 --zip-file fileb://function.zip \
  --role arn:aws:iam::${ACCOUNT_ID}:role/bedrock/bedrock-monthly-unpause
aws scheduler create-schedule --name bedrock-monthly-unpause \
  --schedule-expression 'cron(0 6 1 * ? *)' --schedule-expression-timezone UTC \
  --flexible-time-window Mode=OFF \
  --target "{\"Arn\":\"arn:aws:lambda:us-west-2:${ACCOUNT_ID}:function:bedrock-monthly-unpause\",
             \"RoleArn\":\"arn:aws:iam::${ACCOUNT_ID}:role/bedrock/bedrock-monthly-unpause-scheduler\",
             \"RetryPolicy\":{\"MaximumRetryAttempts\":3}}"
```

**Check:** `aws lambda invoke --function-name bedrock-monthly-unpause out.json && cat out.json`
lists who was unpaused and re-armed. It is safe to run at any time.

---

## Phase 1: Onboard an engineer

**Who:** a manager or admin with IAM rights in the account. Do this once for each engineer.

### Step 1.1 Fill in the engineer's details

```bash
export ACCOUNT_ID=<aws-account-id>
export AWS_PROFILE=<your-admin-sso-profile>

export NAME=<short-name>          # e.g. alice  → role "bedrock-user-alice"
export EMAIL=<sso-login-email>    # exactly what they sign in to SSO with, e.g. alice@example.com
export OWNER="$EMAIL"             # cost tag value; keep it the same as EMAIL
export NOTIFY=<mailbox-email>     # where budget alerts go, e.g. alice.smith@example.com (may differ from EMAIL)
export PRODUCT=<team-or-product>  # e.g. my_product
export LIMIT=<monthly-usd>        # e.g. 100
```

> Use the **same email convention for everyone** in `OWNER` (the SSO login email is recommended).
> Otherwise cost reports may show one person under two names.

### Step 1.2 Create the personal role ✅

```bash
cat > trust-${NAME}.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "AWS": "arn:aws:iam::${ACCOUNT_ID}:root" },
    "Action": "sts:AssumeRole",
    "Condition": {
      "ArnLike":      { "aws:PrincipalArn": "arn:aws:iam::${ACCOUNT_ID}:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_*" },
      "StringLike":   { "aws:userid": "*:${EMAIL}" },
      "StringEquals": { "sts:RoleSessionName": "${EMAIL}" }
    }
  }]
}
EOF
cat > bedrock-invoke.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
      "Resource": ["arn:aws:bedrock:*::foundation-model/*",
                   "arn:aws:bedrock:*:${ACCOUNT_ID}:inference-profile/*"]
    },
    {
      "Effect": "Allow",
      "Action": ["bedrock:ListFoundationModels", "bedrock:GetFoundationModel",
                 "bedrock:ListInferenceProfiles", "bedrock:GetInferenceProfile"],
      "Resource": "*"
    }
  ]
}
EOF
aws iam create-role --role-name bedrock-user-${NAME} --path /bedrock-users/ \
  --assume-role-policy-document file://trust-${NAME}.json \
  --max-session-duration 3600 \
  --tags Key=owner,Value=${OWNER} Key=product,Value=${PRODUCT}
aws iam put-role-policy --role-name bedrock-user-${NAME} \
  --policy-name bedrock-invoke --policy-document file://bedrock-invoke.json
```

Only this engineer, signed in through SSO, can use this role. The role can only call Bedrock.

### Step 1.3 Create the budget and the automatic pause ✅

> ⚠️ **Never create the budget without the per-person filter.** Without it, the budget counts the
> whole account's spend and pauses the engineer because of everyone else's usage.
>
> The filter key is **`iamPrincipal/owner`**, not `owner`. Plain `owner` is the resource tag and
> would count $0.

```bash
aws budgets create-budget --account-id ${ACCOUNT_ID} \
  --budget "{\"BudgetName\":\"bedrock-${NAME}\",\"BudgetType\":\"COST\",\"TimeUnit\":\"MONTHLY\",
             \"BudgetLimit\":{\"Amount\":\"${LIMIT}\",\"Unit\":\"USD\"},
             \"FilterExpression\":{\"Tags\":{\"Key\":\"iamPrincipal/owner\",
                \"Values\":[\"${OWNER}\"],\"MatchOptions\":[\"EQUALS\"]}},
             \"Metrics\":[\"UnblendedCost\"]}" \
  --notifications-with-subscribers "[{\"Notification\":{\"NotificationType\":\"ACTUAL\",
      \"ComparisonOperator\":\"GREATER_THAN\",\"Threshold\":80,\"ThresholdType\":\"PERCENTAGE\"},
      \"Subscribers\":[{\"SubscriptionType\":\"EMAIL\",\"Address\":\"${NOTIFY}\"}]}]"

aws budgets create-budget-action --account-id ${ACCOUNT_ID} --budget-name bedrock-${NAME} \
  --notification-type ACTUAL --action-type APPLY_IAM_POLICY \
  --action-threshold ActionThresholdValue=100,ActionThresholdType=PERCENTAGE \
  --definition "{\"IamActionDefinition\":{\"PolicyArn\":\"arn:aws:iam::${ACCOUNT_ID}:policy/bedrock/bedrock-deny\",\"Roles\":[\"bedrock-user-${NAME}\"]}}" \
  --execution-role-arn arn:aws:iam::${ACCOUNT_ID}:role/bedrock/bedrock-budget-actions \
  --approval-model AUTOMATIC \
  --subscribers SubscriptionType=EMAIL,Address=${NOTIFY}
```

The engineer gets an email at 80% of the budget. At 100% they are paused automatically.

**Check:** `aws budgets describe-budget --account-id ${ACCOUNT_ID} --budget-name bedrock-${NAME}
--query Budget.CalculatedSpend` shows the engineer's spend this month. A new engineer shows $0
until their first Bedrock calls reach billing, about a day later.

> ⚠️ The pause is not instant. Budgets reacts **3 to 16 hours** after the spend crosses the limit
> (measured), so engineers can go somewhat over budget. Set limits with that in mind.

### Step 1.4 Send the engineer their details

Send them the account ID and their role name (`bedrock-user-<name>`), and point them to
[Phase 2](#phase-2-engineer-setup).

---

## Phase 2: Engineer setup

**Who:** the engineer. **Before you start:** you can sign in to the AWS account with SSO, and your
manager has sent you your role name.

### Step 2.1 Sign in with SSO ✅

```bash
aws login                         # or: aws sso login --profile <your-sso-profile>
aws sts get-caller-identity       # the ARN must end in /<your-sso-email>
```

> ⚠️ `aws login` reuses whatever identity your browser console is signed in as. Check the output.

### Step 2.2 Add your Bedrock profile ✅

Add this to `~/.aws/config` and replace **every** `<…>` placeholder:

```ini
[profile bedrock]
role_arn = arn:aws:iam::<ACCOUNT_ID>:role/bedrock-users/bedrock-user-<name>
source_profile = default
role_session_name = <your-sso-email>
region = us-west-2
```

- `source_profile` is the profile you signed in with in Step 2.1 (`default` if you used `aws login`).
- `role_session_name` must be **exactly** your SSO email.
- ⚠️ Don't put comments at the end of a line. Put them on their own line, starting with `#`.

### Step 2.3 Check it ✅

```bash
aws sts get-caller-identity --profile bedrock
# Expect: arn:aws:sts::<ACCOUNT_ID>:assumed-role/bedrock-user-<name>/<your-sso-email>
```

### Step 2.4 Set up Claude Code ✅

In `~/.claude/settings.json`:

```json
{
  "env": {
    "CLAUDE_CODE_USE_BEDROCK": "1",
    "AWS_PROFILE": "bedrock",
    "AWS_REGION": "us-west-2"
  }
}
```

Then restart Claude Code.

- The `env` block here **overrides** your shell. Exporting a different `AWS_PROFILE` in the shell has no effect.
- Your Bedrock role can **only** call Bedrock. For any other AWS command, add `--profile default`.
- Credentials renew automatically. When your SSO sign-in expires, run `aws login` again.

### If something goes wrong

| You see | Fix |
|---|---|
| `AccessDenied … sts:AssumeRole` | A `<…>` placeholder is still in `role_arn`, or `role_session_name` isn't exactly your SSO email |
| `The config profile (default  # …) could not be found` | Move the end-of-line comment in `~/.aws/config` to its own line |
| Claude Code uses the wrong identity | Set `AWS_PROFILE` in `~/.claude/settings.json`, then restart Claude Code |
| `Token has expired` / SSO session expired | Run `aws login` again |
| `AccessDeniedException … bedrock:InvokeModel` (it worked before) | You're paused because you went over budget. Contact your manager. |
| `AccessDenied` on a non-Bedrock command | Expected. Use `--profile default`. |

---

## Phase 3: Day-to-day operations

**Who:** manager or admin. Use the same variables as in Step 1.1.

### Unpause someone ✅

Bedrock access returns **about 20 seconds** after the unpause, once IAM has applied the change.

```bash
ACTION_ID=$(aws budgets describe-budget-actions-for-budget --account-id ${ACCOUNT_ID} \
  --budget-name bedrock-${NAME} --query 'Actions[0].ActionId' --output text)
aws budgets execute-budget-action --account-id ${ACCOUNT_ID} --budget-name bedrock-${NAME} \
  --action-id ${ACTION_ID} --execution-type REVERSE_BUDGET_ACTION
# Re-arm the pause, otherwise it never fires again:
aws budgets execute-budget-action --account-id ${ACCOUNT_ID} --budget-name bedrock-${NAME} \
  --action-id ${ACTION_ID} --execution-type RESET_BUDGET_ACTION
```

> ⚠️ Re-arming during the month while the engineer is still over budget pauses them again a few
> hours later. To give someone more room this month, raise their limit first (below).

Who may approve an exception is still to be decided.

### On the 1st of each month ✅

Nothing to do. The job from [Step 0.6](#step-06-turn-on-the-monthly-auto-unpause-) unpauses and
re-arms everyone at 06:00 UTC.

### Change someone's budget ✅

> ⚠️ `update-budget` replaces the whole budget definition. Always include the filter, as below.

```bash
aws budgets update-budget --account-id ${ACCOUNT_ID} --new-budget \
  "{\"BudgetName\":\"bedrock-${NAME}\",\"BudgetType\":\"COST\",\"TimeUnit\":\"MONTHLY\",
    \"BudgetLimit\":{\"Amount\":\"${LIMIT}\",\"Unit\":\"USD\"},
    \"FilterExpression\":{\"Tags\":{\"Key\":\"iamPrincipal/owner\",
       \"Values\":[\"${OWNER}\"],\"MatchOptions\":[\"EQUALS\"]}},
    \"Metrics\":[\"UnblendedCost\"]}"
```

### Offboard someone

Removing the person from SSO already blocks their access. To clean up:

```bash
aws budgets delete-budget --account-id ${ACCOUNT_ID} --budget-name bedrock-${NAME}
aws iam delete-role-policy --role-name bedrock-user-${NAME} --policy-name bedrock-invoke
aws iam detach-role-policy --role-name bedrock-user-${NAME} \
  --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/bedrock/bedrock-deny 2>/dev/null
aws iam delete-role --role-name bedrock-user-${NAME}
```

### See who spent what

See the reporting query in [bedrock-guideline.md §4.7](bedrock-guideline.md#47-reporting-from-the-cur-export-).
Remember that AWS bills Claude and other models under **AWS Marketplace**, not "Amazon Bedrock".
