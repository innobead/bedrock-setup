# Amazon Bedrock Usage Guideline

> **Status: DRAFT v0.3 (2026-10-03).** Sections marked ✅ were tested in account `111122223333`.
> Sections marked ⏳ are waiting on a test or on IT, and sections marked 📝 are not written yet.
> Proof-of-concept files: [`poc/`](poc/). Guides: [admin](bedrock-admin-guide.md), [user](bedrock-user-guide.md); admin script [`scripts/bedrock-user.sh`](scripts/bedrock-user.sh).

## Contents

1. [Overview](#1-overview)
2. [Key concepts](#2-key-concepts)
3. [Design decisions](#3-design-decisions)
4. [Admin setup](#4-admin-setup)
5. [User setup](#5-user-setup)
6. [Preventing bypass](#6-preventing-bypass)
7. [Operations](#7-operations)
8. [Open items](#8-open-items)

---

## 1. Overview

Employees use Claude and other models through **Amazon Bedrock**. Each person:

- signs in with the company **SSO** (IAM Identity Center),
- uses a **personal IAM role** that is tagged with their identity,
- has a **personal monthly budget**, and
- is **paused automatically** when they exceed it, without affecting anyone else.

### How the pieces fit together

```
 Employee (SSO: alice@example.com)
    │  aws login / aws sso login
    ▼
 SSO session  (AWSReservedSSO_<PermissionSet>_…/alice@example.com)
    │  sts:AssumeRole  (automatic, via ~/.aws/config)
    ▼
 Personal role: role/bedrock-users/bedrock-user-alice
    tags: owner=alice@example.com, product=<team/product>
    │  bedrock:InvokeModel*
    ▼
 Amazon Bedrock  ──►  Billing records caller + role tags
                          │
                          ▼
                     AWS Budget "bedrock-alice"  (filter: iamPrincipal owner = alice@example.com)
                          │  threshold exceeded
                          ▼
                     Budget action: attach policy/bedrock/bedrock-deny
                          to role bedrock-user-alice  →  only Alice is paused
```

---

## 2. Key concepts

### 2.1 SSO users vs. IAM users

| | IAM Identity Center (SSO) | IAM user |
|---|---|---|
| Credentials | Temporary only | Password and/or long-lived access keys |
| Lifecycle | Managed centrally from the IdP (offboarding is automatic) | Managed separately in each account |
| Caller ARN | `…:assumed-role/AWSReservedSSO_<Set>_<id>/alice@example.com` | `…:user/alice` |
| CLI sign-in | `aws sso login`, or `aws login` if the console session is SSO | `aws login` (console password) or access keys |

**Everyone must use SSO. IAM users are not allowed for Bedrock.**

> ⚠️ `aws login` reuses **whatever identity the browser console is signed in as**. After signing in,
> always check your identity with `aws sts get-caller-identity`.

### 2.2 Why SSO alone is not enough for per-person budgets

- Everyone who is assigned a permission set shares **one IAM role** in the account.
- The person's email appears only as the **session name**, at the end of the caller ARN.
- The session name is enough for **reports**: the CUR `line_item_iam_principal` column contains it.
- It is **not enough** for AWS Budgets, which can filter only on tags, not on parts of an ARN.
- A budget action on the shared role would **pause everyone** who uses that role.

So each person needs their **own IAM role**, carrying their own tags.

### 2.3 Resource tags vs. IAM principal tags

| | Resource tag | IAM principal tag |
|---|---|---|
| Answers | *What* was used? | *Who* used it? |
| Tag is set on | The resource (EC2, S3, inference profile…) | The IAM user or role making the call |
| Services | Most AWS services | **Amazon Bedrock only** |
| Bedrock with plain model IDs | ❌ There is no customer resource, so no tag | ✅ |
| Becomes available to activate | After a tagged resource has cost | After the tagged principal has made **≥ 1 Bedrock call**, then up to 24 h |
| CUR 2.0 prefix | `resourceTags/…` | `iamPrincipal/…` |

> ⚠️ The **same key** (e.g. `owner`) can exist as **both** types. They are separate entries in
> *Billing → Cost Allocation Tags* and **each must be activated separately**. For Bedrock budgets,
> always pick the **IAM principal** type.

Reference: [Using IAM principal for cost allocation](https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/iam-principal-cost-allocation.html)

### 2.4 How Bedrock models are billed

Claude and other third-party models are billed through **AWS Marketplace**. Each model has its own
product code, for example `Claude Opus 5.5 (Amazon Bedrock Edition)`, and usage types look like
`USW2-MP:USW2_output_tokens_standard-Units`.

> ⚠️ A budget or report filtered on *Service = Amazon Bedrock* **misses most model spend**.
> Filter on the IAM principal tag, or on *Billing entity = AWS Marketplace*, instead.

---

## 3. Design decisions

| Decision | Chosen | Rejected alternatives and why |
|---|---|---|
| Identity | SSO (IAM Identity Center) | IAM users: long-lived keys, separate lifecycle, can bypass SSO offboarding |
| Per-person cost attribution | IAM principal tags on a personal role | Application inference profiles per user: one profile **per user per model**, too many to manage. SSO session tags: IT may not enable them, and it is unconfirmed whether billing picks them up. |
| Per-person enforcement | Budget action attaches a shared `bedrock-deny` policy to the personal role | Deny on the shared SSO role: pauses everyone, or needs one conditional policy per user (managed-policy quota of 10–20 per role), and Identity Center may overwrite it. |
| Getting into the personal role | **Now:** SSO → `sts:AssumeRole` (role chaining). **Later:** IAM *account access manager* | Account access manager assigns the role directly (no chaining, simpler client config), but IT must enable it in the management account. Switching later only changes the trust policy and the user sign-in steps. |

---

## 4. Admin setup

Placeholders used below:

- `<ACCOUNT_ID>`: the AWS account where Bedrock is used (PoC: `111122223333`)
- `<name>`: a short username (e.g. `alice`)
- `<email>`: the SSO username / email (e.g. `alice@example.com`)

### 4.1 CUR 2.0 export with caller identity ✅

This gives per-person **reporting**. It works even before any tags exist.

1. Go to *Billing and Cost Management → Data Exports → Create* → **Standard data export (CUR 2.0)**.
2. Under **Additional export content**, enable **Include caller identity (IAM principal) allocation data**.
3. Choose Parquet format, hourly granularity, and an S3 destination.

The first delivery can take up to 24 h. The export adds the `line_item_iam_principal` column and the
`iamPrincipal/*` tags inside the `tags` column.

> Note: this increases the number of CUR rows (one row per caller per model), so the files get larger.

### 4.2 Personal role per user ✅

Each role is named `bedrock-user-<name>`, lives under the path **`/bedrock-users/`**, and is tagged with
`owner` and `product`. The path is what the budget-action role and the SCP use to scope permissions.

**Trust policy** ([`poc/trust-alice.json`](poc/trust-alice.json)). All three conditions must match:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "AllowOwnSsoSessionOnly",
      "Effect": "Allow",
      "Principal": { "AWS": "arn:aws:iam::<ACCOUNT_ID>:root" },
      "Action": "sts:AssumeRole",
      "Condition": {
        "ArnLike":      { "aws:PrincipalArn": "arn:aws:iam::<ACCOUNT_ID>:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_*" },
        "StringLike":   { "aws:userid": "*:<email>" },
        "StringEquals": { "sts:RoleSessionName": "<email>" }
      }
    }
  ]
}
```

| Condition | Purpose |
|---|---|
| `aws:PrincipalArn` matches an SSO role | Only Identity Center sessions may assume the role. IAM users and other roles cannot. |
| `aws:userid` = `*:<email>` | For an SSO session, the value is `<RoleId>:<email>`. Identity Center sets the email, so users cannot fake it. |
| `sts:RoleSessionName` = `<email>` | Forces a readable session name in CloudTrail and the CUR. |

**Permissions policy** ([`poc/bedrock-invoke.json`](poc/bedrock-invoke.json)):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "InvokeModels",
      "Effect": "Allow",
      "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
      "Resource": [
        "arn:aws:bedrock:*::foundation-model/*",
        "arn:aws:bedrock:*:<ACCOUNT_ID>:inference-profile/*"
      ]
    },
    {
      "Sid": "DiscoverModels",
      "Effect": "Allow",
      "Action": [
        "bedrock:ListFoundationModels", "bedrock:GetFoundationModel",
        "bedrock:ListInferenceProfiles", "bedrock:GetInferenceProfile"
      ],
      "Resource": "*"
    }
  ]
}
```

> `bedrock:Converse` and `bedrock:ConverseStream` are **not** IAM actions (IAM Access Analyzer reports
> them as invalid). The Converse APIs are authorized by `InvokeModel` / `InvokeModelWithResponseStream`.

**Create the role:**

```bash
aws iam create-role \
  --role-name bedrock-user-<name> \
  --path /bedrock-users/ \
  --assume-role-policy-document file://trust-<name>.json \
  --max-session-duration 3600 \
  --tags Key=owner,Value=<email> Key=product,Value=<product>

aws iam put-role-policy \
  --role-name bedrock-user-<name> \
  --policy-name bedrock-invoke \
  --policy-document file://bedrock-invoke.json
```

> Role chaining (assuming a role from an SSO session) is always capped at **1 hour**. The AWS CLI,
> the SDKs and Claude Code refresh these credentials automatically.

**Verified in the PoC:** assuming the role as the correct user works; the wrong session name is
denied; Bedrock calls succeed; non-Bedrock actions are denied; Claude Code works through the role
(confirmed in CloudTrail).

### 4.3 Activate IAM principal cost allocation tags ✅

> ✅ **Done for all accounts.** Central IT has activated the IAM-principal tags `owner` and `product`
> organization-wide. New accounts need nothing; their personal roles just have to use these
> exact tag names. The steps below are kept for reference.

This is done **in the management account** (only it can manage cost allocation tags).

1. Make at least one Bedrock call through a tagged personal role.
2. Wait up to 24 h.
3. Go to *Billing → Cost Allocation Tags* and **filter Tag type = IAM principal**.
4. Activate `owner` and `product`. Activation can take up to another 24 h.

> ✅ **PoC status:** IT first activated the **resource**-type `owner`/`product` tags by mistake. Those
> do not apply to Bedrock. The **IAM principal** types were then activated on 2026-10-02 at
> 07:10 UTC (the first tagged call was on 2026-09-30 at 16:57 UTC). ✅ The CUR export delivered at
> 04:27 UTC on 2026-10-03 has `iamPrincipal/owner = alice.smith@example.com` and `iamPrincipal/product`
> on every `bedrock-user-alice` row from 2026-10-02 14:00 UTC on. Earlier rows have no tag
> (no backfill was requested). ✅ On 2026-10-03, Cost Explorer showed the tags under the **separate keys
> `iamPrincipal/owner` and `iamPrincipal/product`**, not under `owner`. Filtering on
> `iamPrincipal/owner = alice.smith@example.com` returned only Claude model charges (Opus, Sonnet, Haiku),
> with no resource-tag spend mixed in.

### 4.4 The shared deny policy ✅

A **single** customer managed policy, [`poc/bedrock-deny.json`](poc/bedrock-deny.json), is reused for
every person. It is created as `policy/bedrock/bedrock-deny`.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PauseBedrockModelUsage",
      "Effect": "Deny",
      "Action": [
        "bedrock:InvokeModel",
        "bedrock:InvokeModelWithResponseStream",
        "bedrock:CreateModelInvocationJob",
        "bedrock:CallWithBearerToken"
      ],
      "Resource": "*"
    }
  ]
}
```

**Verified in the PoC:** after the policy was attached to `bedrock-user-alice`, Bedrock calls returned
`AccessDeniedException` within about 15 s. After it was detached, calls worked again. Nobody else
was affected.

### 4.5 Execution role for AWS Budgets ✅ (verified with the budget action)

AWS Budgets uses this role to attach the deny policy. It may **only** attach or detach `bedrock-deny`,
and **only** on roles under `bedrock-users/`.

- Trust policy: [`poc/budget-actions-trust.json`](poc/budget-actions-trust.json). It trusts `budgets.amazonaws.com`, limited to this account's budgets.
- Permissions: [`poc/budget-actions-permissions.json`](poc/budget-actions-permissions.json).

```json
{
  "Effect": "Allow",
  "Action": ["iam:AttachRolePolicy", "iam:DetachRolePolicy"],
  "Resource": "arn:aws:iam::<ACCOUNT_ID>:role/bedrock-users/*",
  "Condition": { "ArnEquals": { "iam:PolicyARN": "arn:aws:iam::<ACCOUNT_ID>:policy/bedrock/bedrock-deny" } }
}
```

The role is created as `role/bedrock/bedrock-budget-actions`.

> ✅ These minimal permissions are sufficient. In the PoC, AWS Budgets used this role to attach
> `bedrock-deny` successfully, and nothing else was needed.

### 4.6 Per-person budget and action ✅

```bash
# 1. Budget, filtered to one person's Bedrock spend
aws budgets create-budget --account-id <ACCOUNT_ID> \
  --budget '{"BudgetName":"bedrock-<name>","BudgetType":"COST","TimeUnit":"MONTHLY",
             "BudgetLimit":{"Amount":"<limit>","Unit":"USD"},
             "FilterExpression":{"Tags":{"Key":"iamPrincipal/owner","Values":["<owner>"],"MatchOptions":["EQUALS"]}},
             "Metrics":["UnblendedCost"]}' \
  --notifications-with-subscribers '[{"Notification":{"NotificationType":"ACTUAL",
      "ComparisonOperator":"GREATER_THAN","Threshold":80,"ThresholdType":"PERCENTAGE"},
      "Subscribers":[{"SubscriptionType":"EMAIL","Address":"<email>"}]}]'

# 2. Action: pause at 100%
aws budgets create-budget-action --account-id <ACCOUNT_ID> --budget-name bedrock-<name> \
  --notification-type ACTUAL --action-type APPLY_IAM_POLICY \
  --action-threshold ActionThresholdValue=100,ActionThresholdType=PERCENTAGE \
  --definition '{"IamActionDefinition":{"PolicyArn":"arn:aws:iam::<ACCOUNT_ID>:policy/bedrock/bedrock-deny","Roles":["bedrock-user-<name>"]}}' \
  --execution-role-arn arn:aws:iam::<ACCOUNT_ID>:role/bedrock/bedrock-budget-actions \
  --approval-model AUTOMATIC \
  --subscribers SubscriptionType=EMAIL,Address=<email>
```

> ✅ **Budget filter:** the tag key is **`iamPrincipal/owner`** in both Cost Explorer and Budgets
> (verified 2026-10-03). Use `FilterExpression` + `Metrics`, the newer Budgets syntax that matches
> Cost Explorer. The real budget `bedrock-alice` ($6) reported **$4.635** right after creation, the
> same as Cost Explorer for that filter. That amount was Claude model charges only (Opus, Sonnet,
> Haiku). Filtering on plain `owner` matches the *resource* tag and returns $0.
>
> ✅ **End-to-end through the filter:** the 80% alert went to `ALARM`, and the pause fired at 04:47 UTC on
> 2026-10-04, about 10.5 h after the budget was created. Afterwards the limit was raised to $100 with
> `update-budget`, keeping the filter.
>
> ✅ **PoC result:** test budget `poc-bedrock-alice` (whole account, $1 limit, already exceeded when it
> was created at 17:08 UTC on 2026-09-30) **fired automatically at 20:24 UTC, about 3 h 15 min later**.
> AWS Budgets attached `bedrock-deny` to `bedrock-user-alice`. Calls through the personal role were
> then denied, while the Admin SSO role kept working, so only the targeted person was paused.
> The action was reversed and the test budget deleted at 03:44 UTC on 2026-10-01, so
> `poc/check-status.sh` no longer has anything to check.

> ⚠️ Budget data lags actual usage by **several hours**. Measured time from "over the limit" to
> "paused": 3 h 15 min, 15.5 h, 5.3 h and 10.5 h in four tests. The pause is a **soft limit**: a person can
> exceed their budget by the amount they spend before the next budget evaluation.

### 4.7 Reporting from the CUR export ✅

A quick local query with DuckDB:

```sql
-- Bedrock / Marketplace model cost per person
SELECT
  split_part(line_item_iam_principal, '/', -1) AS person,   -- session name = email
  product['product_name']                      AS model,
  round(sum(line_item_unblended_cost), 4)      AS cost_usd
FROM read_parquet('cur/**/*.parquet', hive_partitioning = true)
WHERE coalesce(line_item_iam_principal, '') <> ''
GROUP BY ALL
ORDER BY cost_usd DESC;
```

For ongoing reporting, use Athena (a Glue table over the export prefix) or CUDOS dashboards. 📝

---

## 5. User setup

### 5.1 AWS CLI profile ✅

Add the following to `~/.aws/config`. It builds on the SSO sign-in you already use.

Replace **every** `<…>` placeholder with your own values.

```ini
[profile bedrock-personal]
role_arn = arn:aws:iam::<ACCOUNT_ID>:role/bedrock-users/bedrock-user-<name>
source_profile = default
role_session_name = <email>
region = us-west-2
```

- The profile name (`bedrock-personal`) is your choice. Use the same name in Claude Code (5.2).
- `source_profile` is your SSO sign-in profile, the one that `aws login` or `aws sso login` writes.
  ✅ Tested with an `aws login` profile (`login_session = …`) for both the CLI and Claude Code.
- `role_session_name` must be **exactly** your SSO email, or access is denied.
- Credentials from this role last 1 hour (the role-chaining limit). The CLI, the SDKs and
  Claude Code refresh them automatically while your SSO sign-in is still valid.

> ⚠️ **Do not put comments at the end of a line** in `~/.aws/config`. The AWS CLI and SDKs read
> everything after `=` as the value. For example, `source_profile = default  # my SSO` makes the
> CLI look for a profile literally named `default  # my SSO`. Put comments on their own line,
> starting with `#`.

Then verify it:

```bash
aws login                                             # or: aws sso login --profile <sso-profile>
aws sts get-caller-identity --profile bedrock-personal
# Expect: arn:aws:sts::<ACCOUNT_ID>:assumed-role/bedrock-user-<name>/<email>
```

### 5.2 Claude Code ✅

In `~/.claude/settings.json`:

```json
{
  "env": {
    "CLAUDE_CODE_USE_BEDROCK": "1",
    "AWS_PROFILE": "bedrock-personal",
    "AWS_REGION": "us-west-2"
  }
}
```

> ⚠️ The `env` block in `settings.json` **overrides** your shell environment. If `AWS_PROFILE` is set
> here, exporting a different profile in the shell has no effect. Restart Claude Code after you
> change it.

> ℹ️ The personal role can **only** call Bedrock. Any other AWS command run inside Claude Code
> (CloudTrail, S3, IAM, …) fails with `AccessDenied`. That is expected. For other AWS work, pass
> your SSO profile explicitly, for example `aws cloudtrail lookup-events --profile default`.

### 5.3 When you are paused

Bedrock calls fail with `AccessDeniedException … bedrock:InvokeModel`, and you receive an email from
AWS Budgets. The pause applies immediately, including to sessions that are already running. See
[7.2](#72-unpausing--exceptions) for how to get access back. 📝

### 5.4 Troubleshooting ✅

Start by checking who you are: `aws sts get-caller-identity --profile <profile>`.

| Symptom | Likely cause | Fix |
|---|---|---|
| `AccessDenied … sts:AssumeRole` | A `<…>` placeholder is still in `role_arn`, or `role_session_name` is not exactly your SSO email | Fix `~/.aws/config` (5.1) |
| `The config profile (default  # …) could not be found` | A comment at the end of a line in `~/.aws/config` | Move the comment to its own line |
| Claude Code runs as the wrong role (for example the Admin role) | `AWS_PROFILE` in `~/.claude/settings.json` overrides the shell | Set it there (5.2) and restart Claude Code |
| `Token has expired` / `SSO session … expired` | Your SSO sign-in expired | Run `aws login` (or `aws sso login`) again |
| `AccessDeniedException … bedrock:InvokeModel` that used to work | You are paused by your budget | See 5.3 |
| `AccessDenied` on non-Bedrock APIs | Expected: the personal role is Bedrock-only | Use `--profile default` |

---

## 6. Preventing bypass

The personal-role setup only works if people **cannot use Bedrock another way**. In the PoC account,
the following can currently call Bedrock directly, and those calls are untagged:

- the `AWSAdministratorAccess` and `AWSPowerUserAccess` permission sets,
- IAM users,
- long-term Bedrock API keys (these create hidden IAM users).

| Option | Strength | Effort |
|---|---|---|
| **A. Detect:** a monthly CUR query lists Bedrock spend by callers outside `bedrock-users/` | Visibility only | Low |
| **B. Deny in permission sets:** add an inline `Deny bedrock:Invoke*` to the Admin/PowerUser sets, and retire IAM users | Medium (admins can still create roles) | Low (IT, Identity Center) |
| **C. SCP:** allow Bedrock only from `role/bedrock-users/*`, `role/bedrock-apps/*` and a break-glass role, and protect the personal roles from modification | Strong | Medium (management account) |

**Recommendation for the first rollout: A + B. Add C if bypass is observed.** 📝 The draft SCP will be
added in the next revision.

---

## 7. Operations 📝

### 7.1 Onboarding / offboarding
- Onboarding: `scripts/bedrock-user.sh onboard …` creates the role, the budget and the action (tested 2026-10-05).
- Offboarding: delete the role and the budget. SSO deactivation already blocks access.

### 7.2 Unpausing / exceptions

```bash
aws budgets execute-budget-action --account-id <ACCOUNT_ID> --budget-name bedrock-<name> \
  --action-id <action-id> --execution-type REVERSE_BUDGET_ACTION
aws budgets execute-budget-action --account-id <ACCOUNT_ID> --budget-name bedrock-<name> \
  --action-id <action-id> --execution-type RESET_BUDGET_ACTION
```

> ✅ **Reverse leaves the action disarmed.** After `REVERSE_BUDGET_ACTION`, the status is
> `REVERSE_SUCCESS`. In that state the test action did **not** fire again in 27 h, although the spend
> stayed far above the limit. `RESET_BUDGET_ACTION` moves it back to `STANDBY` (tested 2026-10-03).
> ✅ A reset action fires again: the test action was reset at 18:06 UTC on 2026-10-03 and paused its
> role again at 23:26 UTC. After a reverse, Bedrock access returned **about 20 s** later (IAM propagation).

Still to define: who may approve an exception, and how.

### 7.3 Monthly reset ✅

AWS does not lift the pause at the start of a month (see below). The Lambda
`bedrock-monthly-unpause` ([`poc/unpause/`](poc/unpause/)) runs at 06:00 UTC on the 1st, from the
EventBridge Scheduler schedule `cron(0 6 1 * ? *)`. For every `bedrock-*` budget it reverses fired
actions and resets them to `STANDBY`.
- ✅ Tested by hand on 2026-10-02: it reversed a fired action in under 1 s.
- ✅ It does not need `iam:PassRole`. It needs only `budgets:DescribeBudgetActionsForAccount`,
  `budgets:DescribeBudgetAction`, `budgets:ExecuteBudgetAction` and CloudWatch Logs.
- ✅ Full run on 2026-10-05 at 02:31 UTC: it unpaused and re-armed two paused budgets (`STANDBY`, deny
  detached), and Bedrock through the personal role worked again about 20 s later.

**Original observation:** at 03:40 UTC on 2026-10-01, almost 4 h into the new budget period, the action
had **not** been reversed. `bedrock-deny` was still attached, and the action history showed no new
events. **Assume that a paused person stays paused into the next month until an admin reverses
the action.** A monthly unpause step (manual or scripted) is needed. Check again later on
2026-10-01 to see whether AWS resets it later in the day (this needs a new test budget, because
the PoC one was deleted). *(PoC: the observation ended at 03:44 UTC,
when the action was reversed manually with `REVERSE_BUDGET_ACTION`. That worked within 1 s, and the
test budget was then deleted.)*

---

## 8. Open items

| # | Item | Owner | Status |
|---|---|---|---|
| 1 | IAM-principal-type `owner`/`product` tags appear and are activated (now for all accounts) | IT (management account) | ✅ Activated 2026-10-02 07:10 UTC; in CUR and Cost Explorer as `iamPrincipal/owner` from 2026-10-03 |
| 2 | AWS Budgets can filter on the IAM principal `owner` tag | alice | ✅ `FilterExpression` on `iamPrincipal/owner` matches Cost Explorer; the 80% alert and 100% pause fired through it |
| 3 | Budget action fires with the minimal execution role | alice | ✅ Fired 2026-09-30 20:24 UTC, about 3 h after the threshold was exceeded |
| 4 | Behaviour of the action at the monthly reset | alice | ✅ AWS does not unpause or re-arm; the monthly Lambda does (7.3). A reset action fires again. |
| 5 | Negative test: another user (carol) cannot assume `bedrock-user-alice` | alice + carol | ⏳ |
| 6 | Session name and CLI behaviour with account access manager | IT + alice | 📝 Future |
| 7 | Bypass controls (option B or C), plus the draft SCP for section 6 | IT + alice | 📝 |
| 8 | Sections still to write: model access and regions, AI-usage group / permission set, Athena reporting | alice | 📝 |
