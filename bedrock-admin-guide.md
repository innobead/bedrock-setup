# Amazon Bedrock: Admin Guide

> **Status: v0.2 (2026-10-05).** For **admins**: the owners of an AWS account
> where engineers use Bedrock. You set up the account once (Part 1) and then onboard engineers
> (Part 2). Nothing in this guide needs central IT. Engineers follow the [user guide](bedrock-user-guide.md). The design
> and test results are in [bedrock-guideline.md](bedrock-guideline.md).
> ✅ = tested · ⏳ = not ready yet

## How it works, in short

- Each engineer gets a **personal IAM role** (`bedrock-user-<name>`) that can only call Bedrock. They
  reach it from their SSO login.
- The role is tagged with their email. AWS bills each Bedrock call with that tag.
- Each engineer has a **monthly budget** on that tag. At 80% they get an email. At 100% AWS Budgets
  **pauses only them** by attaching a deny policy to their role.
- On the 1st of each month a scheduled job unpauses everyone.

| Part | Who | When |
|---|---|---|
| [Part 1: One-time account setup](#part-1-one-time-account-setup) | Admin | Once per AWS account, ~20 min |
| [Part 2: Managing your engineers](#part-2-managing-your-engineers) | Admin | Per engineer, ~1 min |

---

## Part 1: One-time account setup

**Who:** the account admin.

> The cost allocation tags `owner` and `product` (type *IAM principal*) are already active for all
> accounts. You don't need to request anything. The personal roles must use exactly these two tag
> names, which the onboarding script does.

Set this once in your shell. All commands below use it.

```bash
export ACCOUNT_ID=<aws-account-id>          # e.g. 111122223333
export AWS_PROFILE=<your-admin-sso-profile> # e.g. default
```

### Step 1.1 Create the cost export ✅

This gives per-person cost reports.

1. Open *Billing and Cost Management → Data Exports → Create*.
2. Choose **Standard data export (CUR 2.0)**.
3. Under **Additional export content**, tick **Include caller identity (IAM principal) allocation data**.
4. Choose Parquet, hourly, and an S3 bucket.

### Step 1.2 Create the shared "pause" policy ✅

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

### Step 1.3 Create the role that AWS Budgets uses ✅

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

### Step 1.4 Turn on the monthly auto-unpause ✅

AWS does **not** lift a pause when a new month starts. This scheduled job does it: at 06:00 UTC on
the 1st it unpauses everyone and re-arms their pause for the new month. One job covers all
engineers. Its code is [`scripts/monthly-unpause/lambda_function.py`](scripts/monthly-unpause/lambda_function.py).

```bash
cd scripts/monthly-unpause
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

### Step 1.5 Watch for Bedrock usage outside personal roles ✅

Budgets only cover calls made **through the personal roles**. Calls from other roles (for example
an Admin SSO session) or from IAM users are billed with no `owner` tag, so no budget counts them.

- Don't give engineers IAM users or long-term Bedrock API keys for model access.
- Once a month, check for untagged model spend:

```bash
aws ce get-cost-and-usage --time-period Start=$(date -u +%Y-%m-01),End=$(date -u -v+1d +%Y-%m-%d) \
  --granularity MONTHLY --metrics UnblendedCost \
  --filter '{"Dimensions":{"Key":"BILLING_ENTITY","Values":["AWS Marketplace"]}}' \
  --group-by Type=TAG,Key=iamPrincipal/owner \
  --query 'ResultsByTime[].Groups[].[Keys[0],Metrics.UnblendedCost.Amount]' --output text
```

The line `iamPrincipal/owner$` (no email after `$`) is **untracked** model spend. The other lines
show each engineer. If untracked spend shows up, find out who it is with
[Find who isn't using their personal role](#find-who-isnt-using-their-personal-role-).
(On Linux, use `date -u -d tomorrow +%Y-%m-%d` instead of `date -u -v+1d +%Y-%m-%d`.)


---

## Part 2: Managing your engineers

**Who:** the account admin. **Before you start:** you need admin access to the account, Part 1 must be
done, and you need the script [`scripts/bedrock-user.sh`](scripts/bedrock-user.sh). Run every command with your admin
profile:

```bash
export AWS_PROFILE=<your-admin-sso-profile>   # e.g. default
```

### Onboard an engineer ✅

```bash
scripts/bedrock-user.sh onboard <name> <sso-email> <notify-email> <monthly-usd> <product>

# Example
scripts/bedrock-user.sh onboard alice alice@example.com alice.smith@example.com 100 my_product
```

| Argument | What to put |
|---|---|
| `name` | Short lowercase name. The role is called `bedrock-user-<name>`. |
| `sso-email` | **Exactly** what the engineer signs in to SSO with. Check with them. It is their ID and their cost tag. |
| `notify-email` | The engineer's **real mailbox**, where budget emails go. ⚠️ SSO login emails may not receive mail (e.g. `alice@example.com` vs. `alice.smith@example.com`), so ask for the address they actually read. |
| `monthly-usd` | The monthly limit, e.g. `100` |
| `product` | Team or product for cost reports, e.g. `my_product` |

The script prints the `role_arn` and `role_session_name`. Send those to the engineer together with
the [user guide](bedrock-user-guide.md).

> A new engineer's spend shows as $0 for about a day, until their first calls reach billing.

### Check an engineer's spend and pause status ✅

```bash
scripts/bedrock-user.sh status alice
```

### Change a limit ✅

```bash
scripts/bedrock-user.sh set-limit alice 150
```

### Unpause an engineer ✅

```bash
scripts/bedrock-user.sh unpause alice
```

- Access returns in about **20 seconds**.
- If they are still over budget, they will be **paused again within hours**. To give someone more
  room this month, run `set-limit` first and then `unpause`.
- You don't need to do anything at the start of a month. The scheduled job unpauses everyone.

### Offboard an engineer ✅

```bash
scripts/bedrock-user.sh offboard alice
```

Removing someone from SSO already blocks their access. This removes their role and budget.

### Find who isn't using their personal role ✅

Untracked spend means someone called Bedrock **without their personal role**. The person can still
be identified (below), but **none of the controls apply**:

- no budget, no 80% email and **no automatic pause**, so there is no upper limit;
- no `product` tag, so the cost can't be charged to a team;
- you only find out **after the fact**, from this monthly report, not from Cost Explorer or Budgets;
- if it's a coding agent on the `AWSAdministratorAccess` role, the agent can change anything in the account, not just
  call Bedrock.

The [user guide](bedrock-user-guide.md#why-use-your-personal-role) explains this to engineers. Billing still records **who** made each call (their SSO email or IAM user name), so you can find
them. Run this once a month, or whenever the check in [Step 1.5](#step-15-watch-for-bedrock-usage-outside-personal-roles-)
shows untracked spend:

```bash
pip install duckdb      # once
scripts/untracked-usage.py <s3-export-prefix> [YYYY-MM]

# Example: the export from Step 1.1, current month
scripts/untracked-usage.py s3://my-billing-bucket/cur/my-export
```

`<s3-export-prefix>` is the S3 bucket and path of your export from Step 1.1, ending with the export
name. Example output:

```
Bedrock model spend in 2026-09: tracked $0, untracked $6.43

WHO                            USED VIA                                                   USD  LAST USED
alice@example.com                 SSO role AWSAdministratorAccess                           6.17  2026-09-30
carol                            IAM user                                                  0.26  2026-09-21
```

| `USED VIA` | What happened | What to do |
|---|---|---|
| `SSO role <permission set>` | They used Bedrock directly from their SSO login (for example `AWSAdministratorAccess`) instead of the `bedrock` profile | Ask them to follow the [user guide](bedrock-user-guide.md). If they haven't been onboarded, onboard them. |
| `IAM user` | They used an IAM user or a long-term Bedrock API key | Onboard them, then remove the IAM user's access keys or API key |
| `personal role without owner tag` | The role is missing its `owner` tag | Re-tag it: `aws iam tag-role --role-name bedrock-user-<name> --tags Key=owner,Value=<sso-email>` |
| `role <name>` | An application or another role | Check who owns that role. Apps need their own budget or must be excluded on purpose. |

> - Billing data is about a day behind, so today's calls aren't in the report yet.
> - Calls made before the IAM-principal tags were active also show as untracked. This only matters
>   for history; the tags are now active for all accounts.

### Things to know

- **The pause is not instant.** AWS Budgets pauses someone **3 to 16 hours** after they cross 100%
  (measured), so engineers can go somewhat over budget.
- **Costs appear about a day late** in Budgets and Cost Explorer.
- **To see who spent what:** Cost Explorer → *Group by → Tag → **iamPrincipal/owner***. Plain `owner`
  is a different tag and doesn't include Bedrock. AWS bills Claude and other models under **AWS
  Marketplace**, so a filter on *Service = Amazon Bedrock* misses them.
- Who may approve an exception is still to be decided.
