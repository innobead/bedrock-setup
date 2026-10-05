# Amazon Bedrock: Design and Mechanism

> **Version 1.0 (2026-10-05).** Everything described here was tested end to end in a pilot AWS account.
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
| Bedrock with plain model IDs | No: there is no customer resource to tag | Yes |
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

### 4.1 CUR 2.0 export with caller identity
This gives per-person **reporting**. It works even before any tags exist.

1. Go to *Billing and Cost Management → Data Exports → Create* → **Standard data export (CUR 2.0)**.
2. Under **Additional export content**, enable **Include caller identity (IAM principal) allocation data**.
3. Choose Parquet format, hourly granularity, and an S3 destination.

The first delivery can take up to 24 h. The export adds the `line_item_iam_principal` column and the
`iamPrincipal/*` tags inside the `tags` column.

> Note: this increases the number of CUR rows (one row per caller per model), so the files get larger.

### 4.2 Personal role per user
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

### 4.3 Activate IAM principal cost allocation tags
> **Already done for all accounts.** Central IT has activated the IAM principal tags `owner` and
> `product` organization-wide. New accounts need no action; personal roles must use exactly these
> tag names. The steps below are for reference.

This is done **in the management account** (only it can manage cost allocation tags).

1. Make at least one Bedrock call through a tagged personal role.
2. Wait up to 24 h.
3. Go to *Billing → Cost Allocation Tags* and **filter Tag type = IAM principal**.
4. Activate `owner` and `product`. Activation can take up to another 24 h.

> **Notes**
> - Activate the tags with **Tag type = IAM principal**. The same keys may also be listed as
>   *Resource* tags, which do not apply to Bedrock.
> - Tags apply only to usage after activation. Earlier usage stays untagged unless a backfill is
>   requested.
> - In Cost Explorer and AWS Budgets the tags appear as separate keys, `iamPrincipal/owner` and
>   `iamPrincipal/product`. Plain `owner` is the resource tag and does not include Bedrock spend.

### 4.4 The shared deny policy
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

### 4.5 Execution role for AWS Budgets

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

> These minimal permissions are sufficient: in testing, AWS Budgets attached `bedrock-deny` with this
> role and needed nothing else.

### 4.6 Per-person budget and action
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

> **Budget filter.** Filter on the tag key `iamPrincipal/owner`, using `FilterExpression` and
> `Metrics` (the Budgets syntax that matches Cost Explorer). A filter on plain `owner` matches the
> resource tag and counts $0.
>
> **Test results.** With this filter, the budget's reported spend matched Cost Explorer and contained
> only Claude model charges. The 80% alert and the 100% pause both fired, and only the targeted
> person was paused; other roles kept working. `update-budget` keeps the filter as long as the
> request includes it.

> ⚠️ Budget data lags actual usage by **several hours**. In testing, the time from "over the limit"
> to "paused" ranged from about 3 to 16 hours. The pause is a **soft limit**: a person can
> exceed their budget by the amount they spend before the next budget evaluation.

### 4.7 Reporting from the CUR export
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

For ongoing reporting, use Athena (a Glue table over the export prefix) or CUDOS dashboards.

---

## 5. User setup

The step-by-step instructions for engineers are in the [user guide](bedrock-user-guide.md). The
design points behind them:

- **Role chaining from SSO.** Each engineer adds an AWS CLI profile (named `bedrock` in the guide)
  that assumes `bedrock-user-<name>` from their SSO sign-in (`source_profile`). `role_session_name`
  must be exactly their SSO email, or the trust policy denies access (4.2). An `aws login` profile
  works as the source for both the CLI and Claude Code.
- **Credentials last 1 hour.** This is the role-chaining limit. The CLI, the SDKs and Claude Code
  refresh them automatically while the SSO sign-in is valid.
- **Claude Code uses the built-in `/login` wizard** (3rd-party platform → Amazon Bedrock), with the
  `bedrock` profile selected. Only calls through that profile carry the engineer's cost tags.
- **The wizard writes to `~/.claude/settings.json`**, which every Claude Code session on the machine
  reads, including the Claude desktop app's Code tab. Engineers who also use a personal Claude
  account should keep work in a separate `CLAUDE_CONFIG_DIR`.
- **The personal role can only call Bedrock.** Other AWS commands need the engineer's SSO profile.
- **When paused,** Bedrock calls fail with `AccessDeniedException … bedrock:InvokeModel … explicit
  deny`, including in sessions that are already running. See [7.2](#72-unpausing--exceptions).

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

**Recommendation for the first rollout: A + B. Add C if bypass is observed.** A draft SCP will be
added in a later revision.

---

## 7. Operations
### 7.1 Onboarding / offboarding
- Onboarding: `scripts/bedrock-user.sh onboard …` creates the role, the budget and the action.
- Offboarding: delete the role and the budget. SSO deactivation already blocks access.

### 7.2 Unpausing / exceptions

```bash
aws budgets execute-budget-action --account-id <ACCOUNT_ID> --budget-name bedrock-<name> \
  --action-id <action-id> --execution-type REVERSE_BUDGET_ACTION
aws budgets execute-budget-action --account-id <ACCOUNT_ID> --budget-name bedrock-<name> \
  --action-id <action-id> --execution-type RESET_BUDGET_ACTION
```

> **Always reset after reversing.** After `REVERSE_BUDGET_ACTION`, the action stays in
> `REVERSE_SUCCESS` and does not fire again, even if spend stays above the limit.
> `RESET_BUDGET_ACTION` returns it to `STANDBY`, after which it fires normally. Bedrock access returns
> about 20 seconds after a reverse.

Still to define: who may approve an exception, and how.

### 7.3 Monthly reset

AWS Budgets does not lift a pause or re-arm the action when a new month starts: in testing, a paused
role stayed paused into the next period. The Lambda `bedrock-monthly-unpause`
([`scripts/monthly-unpause/`](scripts/monthly-unpause/)) runs at 06:00 UTC on the 1st (EventBridge
Scheduler, `cron(0 6 1 * ? *)`). For every `bedrock-*` budget it reverses fired actions and resets
them to `STANDBY`.

- It needs only `budgets:DescribeBudgetActionsForAccount`, `budgets:DescribeBudgetAction`,
  `budgets:ExecuteBudgetAction` and CloudWatch Logs permissions. `iam:PassRole` is not required.
- It is safe to run at any time. Actions that have not fired are left unchanged.

---

## 8. Open items

| # | Item | Owner | Status |
|---|---|---|---|
| 1 | Confirm that one engineer cannot assume another engineer's personal role (negative test) | Admin | Planned |
| 2 | Evaluate IAM account access manager (direct role assignment, no role chaining) | IT | Future |
| 3 | Stronger bypass controls (option B or C in section 6), including a draft SCP | IT | Not started |
| 4 | Sections to add: model access and regions, AI-usage permission set, Athena reporting | Admin | Not started |
