# Amazon Bedrock: Design and Mechanism

> **Version 1.0 (2026-10-05).** This document explains how the setup works and why. The steps are in
> the [admin guide](admin-guide.md) and the [user guide](user-guide.md). Everything
> described here was tested end to end in a pilot AWS account.
> Proof-of-concept files: [`poc/`](poc/). Guides: [admin](admin-guide.md), [user](user-guide.md); admin script [`scripts/bedrock-user.sh`](scripts/bedrock-user.sh).

## Contents

1. [Overview](#1-overview)
2. [Key concepts](#2-key-concepts)
3. [Design decisions](#3-design-decisions)
4. [Components and why they are built this way](#4-components-and-why-they-are-built-this-way)
5. [Security model](#5-security-model)
6. [Behaviour and limits](#6-behaviour-and-limits)
7. [Reporting](#7-reporting)
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
 Employee (SSO: achen@example.com)
    │  aws login / aws sso login
    ▼
 SSO session  (AWSReservedSSO_<PermissionSet>_…/achen@example.com)
    │  sts:AssumeRole  (automatic, via ~/.aws/config)
    ▼
 Personal role: role/bedrock-users/bedrock-user-achen
    tags: owner=achen@example.com, product=<team/product>
    │  bedrock:InvokeModel*
    ▼
 Amazon Bedrock  ──►  Billing records caller + role tags
                          │
                          ▼
                     AWS Budget "bedrock-achen"  (filter: iamPrincipal owner = achen@example.com)
                          │  threshold exceeded
                          ▼
                     Budget action: attach policy/bedrock/bedrock-deny
                          to role bedrock-user-achen  →  only Alex is paused
```

---

## 2. Key concepts

### 2.1 SSO users vs. IAM users

| | IAM Identity Center (SSO) | IAM user |
|---|---|---|
| Credentials | Temporary only | Password and/or long-lived access keys |
| Lifecycle | Managed centrally from the IdP (offboarding is automatic) | Managed separately in each account |
| Caller ARN | `…:assumed-role/AWSReservedSSO_<Set>_<id>/achen@example.com` | `…:user/achen` |
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
| Model access | **Claude models only** by default; any Bedrock model on request. The **Claude Fable family is always denied** (not approved). | All models for everyone: less predictable spend, and most use (including Claude Code) is Claude. Per-model budgets: Budgets cannot pause per model. |
| Getting into the personal role | **Now:** SSO → `sts:AssumeRole` (role chaining). **Later:** IAM *account access manager* | Account access manager assigns the role directly (no chaining, simpler client config), but IT must enable it in the management account. Switching later only changes the trust policy and the user sign-in steps. |

---

## 4. Components and why they are built this way

The commands to create each component are in the [admin guide](admin-guide.md). This section
explains what each one does and why.

### 4.1 Cost export with caller identity

*Set up in [admin guide, Step 1.1](admin-guide.md#step-11-create-the-cost-export).*

A CUR 2.0 export with **caller identity (IAM principal) data** adds the column
`line_item_iam_principal`, the ARN of the caller of each Bedrock call, and the `iamPrincipal/*` tags
inside the `tags` column. The caller ARN of a personal role ends in the engineer's SSO email, so the
export identifies **who** made every call, including calls made outside personal roles, which no
budget can see. It is the only place where that information is available: Cost Explorer cannot
group by caller.

The export has one row per caller per model and hour, so the files are larger than a plain CUR.
Exporting only the columns the reports need keeps them small.

### 4.2 Personal role per engineer

*Created by `scripts/bedrock-user.sh onboard` ([admin guide, Part 2](admin-guide.md#onboard-an-engineer)).*

Each engineer has a role `bedrock-user-<name>` under the path **`/bedrock-users/`**, tagged with
`owner` (their SSO email) and `product` (their team). The path lets the Budgets role and any future
SCP target exactly these roles. The tags are what billing records on each call.

**Trust policy.** All three conditions must match:

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
        "StringLike":   { "aws:userid": "*:<sso-email>" },
        "StringEquals": { "sts:RoleSessionName": "<sso-email>" }
      }
    }
  ]
}
```

| Condition | Purpose |
|---|---|
| `aws:PrincipalArn` matches an SSO role | Only Identity Center sessions may assume the role. IAM users and other roles cannot. |
| `aws:userid` = `*:<sso-email>` | For an SSO session, the value is `<RoleId>:<sso-email>`. Identity Center sets it, so nobody can fake it. |
| `sts:RoleSessionName` = `<sso-email>` | Makes the session name, and so the caller ARN in CloudTrail and the cost export, show the engineer's email. |

**Permissions.** The role can only call Bedrock. By default it can invoke **Claude models only**:

```json
"Resource": [
  "arn:aws:bedrock:*::foundation-model/anthropic.claude-*",
  "arn:aws:bedrock:*:<ACCOUNT_ID>:inference-profile/*anthropic.claude-*"
]
```

- A call through a cross-region inference profile (`us.anthropic.claude-…`, `global.anthropic.claude-…`)
  is authorized against both the profile and the underlying model in each region, so both lines are
  needed.
- Global models have an ARN with an empty region (`arn:aws:bedrock:::foundation-model/…`). The `*`
  in the region position also matches that.
- Engineers who need other models get `foundation-model/*` and `inference-profile/*` instead
  (`--all-models` or `set-models … all`).
- **The Claude Fable family is not approved.** Both variants include an explicit deny on
  `foundation-model/anthropic.claude-fable*` and `inference-profile/*anthropic.claude-fable*`. An
  explicit deny overrides any allow, so widening the model access never enables Fable.
- The role can also list and describe models and profiles, so tools can discover what is available.
- `bedrock:Converse` and `bedrock:ConverseStream` are not IAM actions. The Converse APIs are
  authorized by `InvokeModel` and `InvokeModelWithResponseStream`.

**How engineers reach the role.** Engineers sign in to SSO and use an AWS CLI profile (`bedrock` in
the [user guide](user-guide.md)). The profile assumes the role from the SSO session
(`source_profile`) with their SSO email as the session name. Claude Code uses the same profile,
selected in its `/login` wizard. An `aws login` profile works as the source for both the CLI and
Claude Code.

### 4.3 Cost allocation tags

Billing only reports a tag after it is **activated**, and only the management account can do that.
Central IT has activated the IAM principal tags `owner` and `product` for all accounts, so no
action is needed per account. Personal roles must use exactly these tag names.

- The tags must be activated as **Tag type = IAM principal**. The same keys may also exist as
  *Resource* tags, which do not apply to Bedrock.
- Tags apply only to usage after activation. Earlier usage stays untagged unless a backfill is
  requested.
- In Cost Explorer and AWS Budgets the tags appear as separate keys, `iamPrincipal/owner` and
  `iamPrincipal/product`. Plain `owner` is the resource tag and does not include Bedrock spend.

### 4.4 Shared pause policy

*Set up in [admin guide, Step 1.2](admin-guide.md#step-12-create-the-shared-pause-policy).*

One customer managed policy, `policy/bedrock/bedrock-deny`, is used for everyone. Attaching it to a
personal role pauses that engineer, and detaching it lifts the pause. An explicit deny overrides
the role's allow, and it applies to sessions that are already running.

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

The four actions cover on-demand and streaming calls, batch jobs, and calls with a Bedrock API key.
A shared policy is used because a budget action can only attach a fixed, existing policy, and
because one policy per person would quickly reach IAM's managed-policy quotas.

### 4.5 Execution role for AWS Budgets

*Set up in [admin guide, Step 1.3](admin-guide.md#step-13-create-the-role-that-aws-budgets-uses).*

AWS Budgets attaches the pause policy using the role `role/bedrock/bedrock-budget-actions`. It trusts
only `budgets.amazonaws.com` for this account's budgets, and it may only attach or detach
`bedrock-deny`, and only on roles under `/bedrock-users/`:

```json
{
  "Effect": "Allow",
  "Action": ["iam:AttachRolePolicy", "iam:DetachRolePolicy"],
  "Resource": "arn:aws:iam::<ACCOUNT_ID>:role/bedrock-users/*",
  "Condition": { "ArnEquals": { "iam:PolicyARN": "arn:aws:iam::<ACCOUNT_ID>:policy/bedrock/bedrock-deny" } }
}
```

Even if a budget action were misconfigured, this role cannot change any other role or attach any
other policy. In testing, these permissions were sufficient.

### 4.6 Per-person budget and automatic pause

*Created by `scripts/bedrock-user.sh onboard`.*

Each engineer has a monthly cost budget `bedrock-<name>` with:

- a **filter** on the tag `iamPrincipal/owner` = their SSO email, using the `FilterExpression` and
  `Metrics` syntax (the same filter syntax as Cost Explorer);
- an **80% alert** by email to the engineer's real mailbox;
- an **automatic action at 100%** that attaches `bedrock-deny` to their role, run by the role in 4.5.

The filter is what makes the budget per person: without it, a budget counts the whole account and
would pause the engineer for everyone's spend. In testing, the filtered budget matched Cost Explorer,
contained only Claude model charges, and paused only the targeted engineer.

### 4.7 Monthly unpause

*Set up in [admin guide, Step 1.4](admin-guide.md#step-14-turn-on-the-monthly-auto-unpause).*

AWS Budgets does **not** lift a pause when a new month starts. In testing, a paused role stayed
paused into the next period. In addition:

- **Reversing** a fired action (`REVERSE_BUDGET_ACTION`) detaches the deny policy but leaves the
  action in `REVERSE_SUCCESS`, where it never fires again, even if spend stays above the limit.
- **Resetting** it (`RESET_BUDGET_ACTION`) returns it to `STANDBY`, after which it fires normally.

The Lambda `bedrock-monthly-unpause` ([`scripts/monthly-unpause/`](scripts/monthly-unpause/)) runs at
06:00 UTC on the 1st (EventBridge Scheduler, `cron(0 6 1 * ? *)`). For every `bedrock-*` budget it
reverses fired actions and resets them. It needs only `budgets:DescribeBudgetActionsForAccount`,
`budgets:DescribeBudgetAction`, `budgets:ExecuteBudgetAction` and CloudWatch Logs permissions
(`iam:PassRole` is not required), and it is safe to run at any time. `bedrock-user.sh unpause` does
the same for one engineer.

---

## 5. Security model

| Who | Can use a personal role? |
|---|---|
| The engineer, signed in to SSO with the email in the trust policy | Yes, their own role only |
| Another engineer, with any session name | No |
| An account admin (for example `AWSAdministratorAccess`) | No: trust policy conditions apply to every caller |
| IAM users, other roles, or a personal role chaining into another | No: only SSO sessions are trusted |

All of these cases were tested, including another person using the role owner's email as the session
name. The protection does not depend on admin rights, because the `aws:userid` condition checks a
value that Identity Center sets.

**What the trust policy cannot prevent.** An account admin can **edit** a trust policy or a role's
tags, and can call Bedrock directly instead of through a personal role. The Fable deny is part of
the personal-role policy only, so calls from other roles are not covered by it. Blocking Fable for
the whole organization needs option B or C below. Calls outside personal roles
are billed without an `owner` tag, so no budget counts them. In the pilot account, the following
could call Bedrock directly:

- the `AWSAdministratorAccess` and `AWSPowerUserAccess` permission sets,
- IAM users,
- long-term Bedrock API keys (these create hidden IAM users).

| Option | Strength | Effort |
|---|---|---|
| **A. Detect:** a monthly report of Bedrock spend by callers outside personal roles ([admin guide](admin-guide.md#find-who-isnt-using-their-personal-role)) | Visibility only | Low |
| **B. Deny in permission sets:** add an inline `Deny bedrock:Invoke*` to the Admin and PowerUser sets, and retire IAM users | Medium (admins can still create roles) | Low (IT, Identity Center) |
| **C. SCP:** allow Bedrock only from `role/bedrock-users/*`, `role/bedrock-apps/*` and a break-glass role, and protect the personal roles from modification | Strong | Medium (management account) |

**Recommendation for the first rollout: A + B. Add C if bypass is observed.** A draft SCP will be
added in a later revision.

---

## 6. Behaviour and limits

| Topic | Behaviour |
|---|---|
| Billing delay | Cost Explorer, Budgets and the cost export are about a day behind actual usage. |
| Pause delay | The pause is a **soft limit**. In testing, the time from "over the limit" to "paused" ranged from about 3 to 16 hours, so engineers can go somewhat over budget. |
| Pause effect | Takes effect within seconds of the policy change, including in running sessions. Access returns about 20 seconds after an unpause. |
| Re-arming | A reversed pause never fires again until it is reset (4.7). The script and the monthly Lambda always do both. |
| Untagged usage | Calls outside personal roles have no budget, no alert and no pause, and cannot be charged to a team. They are only found after the fact (5, option A). |
| Credentials | Role chaining limits personal-role credentials to **1 hour**. The CLI, the SDKs and Claude Code refresh them automatically while the SSO sign-in is valid. |
| Personal role scope | Bedrock only. Other AWS commands need the engineer's SSO profile. |
| Claude Code settings | The `/login` wizard writes to `~/.claude/settings.json`, which every Claude Code session on the machine reads, including the Claude desktop app's Code tab. Engineers who also use a personal Claude account should keep work in a separate `CLAUDE_CONFIG_DIR`. |
| `aws login` | Reuses whatever identity the browser console is signed in as. Engineers should check with `aws sts get-caller-identity`. |

---

## 7. Reporting

- **Cost Explorer and Budgets** show spend per engineer (tag `iamPrincipal/owner`), per model
  (*Service*) and per team (tag `iamPrincipal/product`). See
  [See who spent what in the console](admin-guide.md#see-who-spent-what-in-the-console).
- **Who used Bedrock outside a personal role** is only in the cost export. The admin script
  `scripts/untracked-usage.sh` reports it ([admin guide](admin-guide.md#find-who-isnt-using-their-personal-role)).
- For ad hoc questions, query the export directly, for example with DuckDB:

```sql
-- Bedrock / Marketplace model cost per person and model
SELECT
  split_part(line_item_iam_principal, '/', -1) AS person,   -- session name = SSO email
  product['product_name']                      AS model,
  round(sum(line_item_unblended_cost), 4)      AS cost_usd
FROM read_parquet('cur/**/*.parquet', hive_partitioning = true)
WHERE coalesce(line_item_iam_principal, '') <> ''
GROUP BY ALL
ORDER BY cost_usd DESC;
```

For ongoing reporting, use Athena (a Glue table over the export prefix) or CUDOS dashboards.

---

## 8. Open items

| # | Item | Owner | Status |
|---|---|---|---|
| 1 | Decide who may approve an exception (unpausing mid-month or raising a limit), and how | Management | Not started |
| 2 | Evaluate IAM account access manager (direct role assignment, no role chaining) | IT | Future |
| 3 | Stronger bypass controls (option B or C in section 5), including a draft SCP | IT | Not started |
| 4 | Sections to add: model access and regions, AI-usage permission set, Athena reporting | Admin | Not started |
