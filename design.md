# Amazon Bedrock: design

How and why per-person Bedrock budgets work, for reviewers in IT, security and finance. To set it
up, see the [admin guide](admin-guide.md). Users follow the [user guide](user-guide.md).

## Contents

1. [Goals](#1-goals)
2. [Key concepts](#2-key-concepts)
3. [Desired state: `bedrock.yaml`](#3-desired-state-bedrockyaml)
4. [Account setup steps](#4-account-setup-steps)
5. [Personal roles](#5-personal-roles)
6. [Budgets and the pause](#6-budgets-and-the-pause)
7. [Pause states and limit changes](#7-pause-states-and-limit-changes)
8. [The monthly Lambda and snapshots](#8-the-monthly-lambda-and-snapshots)
9. [Usage and untracked spend](#9-usage-and-untracked-spend)
10. [The `id`: what `apply` may change](#10-the-id-what-apply-may-change)
11. [Security model](#11-security-model)
12. [Behavior and limits](#12-behavior-and-limits)
13. [Testing](#13-testing)
14. [Implementation and release](#14-implementation-and-release)
15. [Later and open items](#15-later-and-open-items)

## 1. Goals

- Each user has a monthly Bedrock budget. When they reach it, only they are paused.
- One command, `bedrock-admin apply`, sets up the whole account. It is safe to re-run.
- Users are managed by editing one file, reviewed in Git and applied by `apply`.
- One view of everyone's spend, this month or past months, including spend outside the budgets.
- Direct model calls outside personal roles can be blocked, not only reported.
- Configuration (who has access, limits) lives only in the file. Runtime actions (`pause`,
  `unpause`) stay commands, and re-applying the file never undoes them.
- Raising a paused user's limit in the file is enough to restore their access.
- Everything is headless: flags, `--json`, `--yes` and meaningful exit codes (0 ok, 1 error, 2 ran
  fine but found a problem).

There are two CLIs:

| CLI | For | Commands |
| --- | --- | --- |
| `bedrock-admin` | The account admin | `configure`, `doctor`, `plan`, `apply`, `export`, `uninstall`, `pause`, `unpause`, `usage` |
| `bedrock` | Users | `setup`, `doctor`, `claude` |

## 2. Key concepts

### SSO users and IAM users

| | IAM Identity Center (SSO) | IAM user |
| --- | --- | --- |
| Credentials | Temporary only | Password or long-lived access keys |
| Lifecycle | Managed centrally from the identity provider; offboarding is automatic | Managed separately in each account |
| Caller ARN | `...:assumed-role/AWSReservedSSO_<Set>_<id>/achen@example.com` | `...:user/achen` |
| CLI sign-in | `aws sso login`, or `aws login` | Access keys or `aws login` |

Everyone uses SSO. IAM users are not used for Bedrock.

### Why SSO alone is not enough for per-person budgets

- Everyone assigned a permission set shares one IAM role in the account.
- The person's email appears only as the session name, at the end of the caller ARN.
- The session name is enough for reports: the cost export's `line_item_iam_principal` column has
  it.
- It is not enough for AWS Budgets, which filters on tags, not on parts of an ARN.
- A budget action on the shared role would pause everyone who uses it.

So each person gets their own IAM role, with their own tags.

### Resource tags and IAM principal tags

| | Resource tag | IAM principal tag |
| --- | --- | --- |
| Answers | What was used? | Who used it? |
| Set on | The resource (EC2, S3, inference profile) | The IAM role or user making the call |
| Services | Most AWS services | Amazon Bedrock only |
| Can be activated | After a tagged resource has cost | After the tagged principal has made at least one Bedrock call, then up to 24 hours |
| CUR 2.0 key | `resourceTags/...` | `iamPrincipal/...` |

The same key, for example `owner`, can exist as both types. They are separate cost allocation tags
and must be activated separately. The budgets use the IAM principal tag `iamPrincipal/owner`.
Reference: [Using IAM principal for cost allocation](https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/iam-principal-cost-allocation.html).

### How Claude models are billed

Claude models are billed through AWS Marketplace, one product per model, for example
`Claude Sonnet 4.5 (Amazon Bedrock Edition)`. A budget or report filtered on the service "Amazon
Bedrock" misses most model spend. The budgets filter on the owner tag instead, which covers
Marketplace charges too.

### Design decisions

| Decision | Chosen | Rejected and why |
| --- | --- | --- |
| Identity | SSO | IAM users: long-lived keys, separate lifecycle, not covered by SSO offboarding |
| Per-person cost attribution | IAM principal tag on a personal role | Application inference profiles per user: one per user per model, too many. SSO session tags: need IT changes, and billing support is unconfirmed. |
| Per-person enforcement | A budget action attaches a shared deny policy to the personal role | A deny on the shared SSO role: pauses everyone, or needs one conditional policy per user, and Identity Center may overwrite it |
| Model access | Anthropic Claude models; the Claude Fable family is always denied (not approved) | All models: less predictable spend, and most use is Claude. Per-model budgets: Budgets can't pause per model. |
| Getting into the personal role | SSO, then `sts:AssumeRole` (role chaining) | IAM account access manager: no chaining, but IT must enable it in the management account. See [Later](#15-later-and-open-items). |
| Configuration | A desired-state file and `plan`/`apply` | Copy-paste commands and per-user scripts: easy to drift, no review |
| Reading spend | The cost export's Parquet files, read directly | Cost Explorer: can't group by caller. A query engine (DuckDB): an extra install for four columns. |

## 3. Desired state: `bedrock.yaml`

One file describes the account and its users. It has no secrets, only names, so it can live in
Git. `bedrock-admin plan` compares it with the account, and `bedrock-admin apply` makes the account
match.

```yaml
id: acme-prod                   # apply tags what it creates and only changes resources with this id
account: "111122223333"
region: us-west-2
profile: bedrock-admin          # AWS CLI profile
product: genai-tools            # product tag on every personal role

models:                         # inference profiles checked by apply and doctor
  - us.anthropic.claude-opus-5-5
  - us.anthropic.claude-sonnet-5-5
  - us.anthropic.claude-haiku-5-5

defaults:
  limit_usd: 50
  pause_at_percent: 100         # pause when actual spend reaches this % of the limit
  alert_at_percent: [80]        # email alerts on actual spend; [] for none
  notify: owner                 # the user's email, or a fixed address

cost_export:
  bucket: bedrock-cur-111122223333

block_direct_calls:
  enabled: true
  method: permission-set        # or scp
  admin_permission_set: BedrockAdmin

users:
  - email: achen@example.com
  - email: bkim@example.com
    limit_usd: 100
    pause_at_percent: 110
```

- Which file: `-f`, then `$BEDROCK_ADMIN_CONFIG`, then `./bedrock.yaml`, then
  `~/.config/bedrock-admin/bedrock.yaml`. Flags win over environment variables, which win over the
  file.
- The user's name is derived from the email unless `name:` is set. It names the role
  (`bedrock-user-<name>`) and the budget (`bedrock-<name>`).
- The trigger is `limit_usd × pause_at_percent / 100`. Both are percentages on the budget, so the
  trigger follows the limit. `pause_at_percent` is 1 to 200. Forecast-based pauses and fixed-dollar
  thresholds are not offered: forecasts are noisy early in the month, and fixed amounts would not
  follow the limit.
- `models:` doesn't control access; the personal-role policy does (Claude except Fable). A Claude model not in `models:` is not blocked, only not enabled or checked. Listing a model personal roles can't call is a config error.
- The full list of fields and defaults is in the
  [admin guide](admin-guide.md#2-create-bedrockyaml).

### Configuration and runtime actions

| | Configuration, in `bedrock.yaml` | Runtime actions, as commands |
| --- | --- | --- |
| What | Who has access, limit, pause and alert thresholds, notify address, name, account settings | `pause`, `unpause` |
| Changed by | Editing the file, then `plan` and `apply` | Running the command |
| Lasts | Until the file changes | Until AWS Budgets, the monthly Lambda or another command changes it |
| Record | Git history | `--reason`, saved as tags on the role, plus CloudTrail |

Pause state is not in the file. The only way the file affects it is a change to the trigger (see
[limit changes](#limit-changes)). Urgent runtime actions skip review on purpose: they are temporary,
and the record is the role tag and CloudTrail. Pause overrides in the file (for example an
"unpaused until" date) were rejected: a date left in the file is easy to forget.

## 4. Account setup steps

`plan` and `apply` handle these steps, in this order, then the users.

| Step | Creates or checks | Why |
| --- | --- | --- |
| 1.1 | A CUR 2.0 cost export with caller identity, and its private bucket (writable only by Data Exports) | The only source of who made each call, including calls no budget sees. Cost Explorer can't group by caller. |
| 1.2 | The `bedrock-deny` policy (path `/bedrock/`) | The pause: one shared policy that denies `bedrock:InvokeModel`, `InvokeModelWithResponseStream`, `CreateModelInvocationJob` and `CallWithBearerToken` |
| 1.3 | The budget actions role `bedrock-budget-actions` | AWS Budgets needs a role to attach the policy. It may only attach and detach `bedrock-deny` on roles under `/bedrock-users/`. |
| 1.4 | The monthly Lambda and its schedule (06:00 UTC on the 1st) | Re-arms every pause each month, after saving a snapshot. See [section 8](#8-the-monthly-lambda-and-snapshots). |
| 1.5 | Model access: a first call to each model in `models:` | Some models need a first call, from the account, before anyone can use them |
| 1.6 | The `iamPrincipal/owner` cost allocation tag is active | Budgets can only filter on active tags. It can be activated only after a personal role has made a Bedrock call, so it may wait up to a day after the first user starts. Only the management account can activate it; from a member account `plan` shows "needs org admin". |
| 1.7 | Model calls outside personal roles are blocked | See below |

If an account step fails, `apply` skips the users. Step 1.6 shows "waiting for first use" until
the tag can be activated. Steps that need the org admin (1.7, and 1.6 outside the management account)
stay pending; the rest of `apply` continues, and `apply` exits 2 while something is pending.

### Step 1.7: block direct model calls

`bedrock-admin` runs with account-level admin rights only. It has no AWS Organizations permission.
Both ways to apply the deny are managed from the Organizations management account (or a delegated
admin): a service control policy (SCP), or an inline policy on the permission sets. So Step 1.7 is
a handoff:

1. `plan` shows Step 1.7 as "needs org admin". `apply` writes the policy to
   `bedrock-block-policy.json` in the current directory and prints instructions for the org admin,
   for both methods:
   - SCP on the Bedrock account. Preferred: it covers every permission set, including future ones.
     It has no effect if the Bedrock account is the management account.
   - Permission sets: add the statement to each permission set assigned to the account, except
     `admin_permission_set`, and re-provision.
2. The org admin applies it, and the admin sets `block_direct_calls.method` to match.
3. With `method: permission-set`, `plan` and `doctor` read the inline policy of each
   `AWSReservedSSO_*` role in the account and list any permission set missing the deny (other than
   the admin set). With `method: scp`, SCPs can't be read from a member account, so `plan` shows it
   as applied by SCP and not checkable.

The policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "BedrockOnlyViaPersonalRole",
    "Effect": "Deny",
    "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream",
               "bedrock:CreateModelInvocationJob", "bedrock:CallWithBearerToken",
               "bedrock:InvokeAgent", "bedrock:InvokeFlow", "bedrock:RetrieveAndGenerate"],
    "Resource": "*",
    "Condition": {
      "ArnLike": {"aws:PrincipalArn":
        "arn:aws:iam::*:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_*"},
      "ArnNotLike": {"aws:PrincipalArn":
        "arn:aws:iam::*:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_BedrockAdmin_*"}
    }
  }]
}
```

The users' permission sets must also allow `sts:AssumeRole` on `role/bedrock-users/*`. The personal
role trusts the account root, so the SSO role needs that permission itself. That is also an org
admin change. `plan` and `doctor` check it the same way (`AdministratorAccess` and
`PowerUserAccess` count as allowing it), and `apply` includes it in the handoff when it's missing.

`enabled: false` only skips the step. Removing a deny that is in place is also the org admin's job.

The deny doesn't cover:

- Users who can create IAM users, roles, access keys or long-term Bedrock API keys, or pass roles
  to Lambda or EC2. Keep these rights out of users' permission sets.
- Admin-level permission sets, which can remove the deny. The untracked section of `usage` is the
  safety net.

The real checks are end to end: `user-access.robot` makes a direct SSO call and expects a deny, and
`usage` lists any spend from SSO roles.

## 5. Personal roles

Each user gets one role, `bedrock-user-<name>`, under the path `/bedrock-users/`.

| Property | Value | Why |
| --- | --- | --- |
| Tag `owner` | The user's SSO email | Billing records it on every call as `iamPrincipal/owner`; the budget filters on it |
| Tag `product` | `product` from the file | Charge spend to a team or product |
| Tag `bedrock-admin:id` | The file's `id` | Ownership (see [section 10](#10-the-id-what-apply-may-change)) |
| Inline policy `bedrock-invoke` | Call and stream `anthropic.claude-*` foundation models and inference profiles; list and read models; deny the Claude Fable family | Bedrock only, Claude only |
| Max session | 1 hour | The limit for role chaining |

### Trust policy

The role trusts the account root, with three conditions that must all hold:

```json
{
  "Effect": "Allow",
  "Principal": {"AWS": "arn:aws:iam::111122223333:root"},
  "Action": "sts:AssumeRole",
  "Condition": {
    "ArnLike":      {"aws:PrincipalArn": "arn:aws:iam::111122223333:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_*"},
    "StringLike":   {"aws:userid": "*:achen@example.com"},
    "StringEquals": {"sts:RoleSessionName": "achen@example.com"}
  }
}
```

- `aws:PrincipalArn`: only SSO roles may assume it, not IAM users or other roles, and not another
  personal role.
- `aws:userid`: Identity Center sets the part after the colon to the user's SSO email. The caller
  can't choose it.
- `sts:RoleSessionName`: the session name must be the email, so the caller ARN of every call ends
  in the email, in CloudTrail and in the cost export.

`bedrock setup` writes an AWS CLI profile that uses the SSO profile as `source_profile` and sets
`role_session_name` to the SSO email. `bedrock doctor` checks each part. `bedrock claude` runs
`claude -p` once with the profile, in a temporary empty `CLAUDE_CONFIG_DIR` (no tools, MCP servers,
hooks or plugins), to show it works, then prints the Claude Code settings (`env` block and
`awsAuthRefresh`). It also prints the values for the Claude desktop app's Amazon Bedrock settings (region, profile, AWS CLI
path, model list). It never writes Claude Code's settings: the user adds them, or uses `/setup-bedrock`.

## 6. Budgets and the pause

Each user has a monthly cost budget, `bedrock-<name>`:

- Limit: `limit_usd`. Filter: the cost allocation tag `iamPrincipal/owner` equals the user's email.
  Metric: unblended cost.
- One email notification on actual spend for each `alert_at_percent`, sent to the `notify`
  address.
- One budget action: at `pause_at_percent` of actual spend, with automatic approval, attach
  `bedrock-deny` to the user's role, run by the budget actions role. It also emails the `notify`
  address.

`apply` updates the limit, the notifications and the action's threshold in place. It never
recreates the action for a limit change, because a new action is armed.

The pause is a deny, so it wins over the role's allow and takes effect within seconds, including in
running sessions. The user sees `AccessDeniedException ... explicit deny`.

`bedrock-admin pause` attaches `bedrock-deny` to the role directly, without the budget action, and
tags the role with `bedrock:paused-by`, `bedrock:paused-at` and `bedrock:pause-reason`. `unpause`
records `bedrock:unpaused-by`, `-at` and `-reason`. Every change is also in CloudTrail.

## 7. Pause states and limit changes

Pause state lives in each user's budget action, plus the `bedrock:paused-by` tag for pauses by hand.
AWS Budgets, `pause`/`unpause`, the monthly Lambda and, in one case, `apply` change it.

| State | Recorded as | Meaning | Shown as |
| --- | --- | --- | --- |
| Armed | Action `STANDBY` or `PENDING` | Access on; pauses at the trigger | `armed` |
| Paused (over the limit) | Action `EXECUTION_SUCCESS` | AWS Budgets attached `bedrock-deny` | `PAUSED (date)` |
| Paused by hand | `bedrock:paused-by` tag; `bedrock-deny` attached directly | An admin paused them; the action is untouched | `PAUSED by hand (date: reason)` |
| Off for the month | Action `REVERSE_SUCCESS` | Access on; won't pause again this month | `off until <1st>` |

| Event | From | To |
| --- | --- | --- |
| Spend reaches the trigger | Armed | Paused (over the limit) |
| `pause` | Any | Paused by hand |
| `unpause` | Paused by hand | Back to the action's state (usually armed) |
| `unpause` | Paused (over the limit) | Off for the month |
| `unpause` | Armed or off | Unchanged ("not paused", exit 0) |
| `apply` raises the trigger above spend | Paused (over the limit) | Armed, at the new trigger |
| `apply`, any other change | Any | Unchanged |
| Monthly Lambda on the 1st | Paused (over the limit) or off | Armed (snapshot saved first) |
| Monthly Lambda on the 1st | Paused by hand | Unchanged |

Why `unpause` turns the pause off for the month instead of re-arming it: re-arming would pause the
user again within hours, because their spend is already over the trigger.

### Limit changes

What `apply` does when `limit_usd` or `pause_at_percent` changes depends on the state. What matters
is the new trigger. "Spend" is the actual spend AWS Budgets reports, the figure that triggers the
pause.

| State before | Change | `apply` does | `plan` notes |
| --- | --- | --- | --- |
| Armed | Any | Updates the limit; the pause fires at the new trigger | |
| Armed | New trigger at or below spend | Same; AWS Budgets pauses them within hours | `warning: spent $X, will be paused` |
| Paused (over the limit) | New trigger above spend | Updates the limit, unpauses and re-arms at the new trigger | `paused → unpaused, spent $X` |
| Paused (over the limit) | New trigger at or below spend | Updates the limit; stays paused | `still over: spent $X, stays paused` |
| Paused by hand | Any | Updates the limit; stays paused | `paused by hand, unchanged` |
| Off for the month | Any | Updates the limit; stays off until the 1st | `pause off until <1st>, unchanged` |

- The unpause reverses the action, waits, then resets it to armed. It records
  "limit raised to $X in bedrock.yaml" as the reason on the role.
- Billing lags 3 to 16 hours, so more spend may arrive after the unpause. If it passes the new
  trigger, the user is paused again, which is intended.
- There is no option to skip the unpause: the file is the source of truth for limits. To keep
  someone paused anyway, use `pause --reason`, which `apply` leaves alone.
- A budget action changed outside the file must be recreated, which re-arms it. `apply` refuses
  without `--yes`.
- Removing a user and adding them back creates a new, armed action. Removals need `--yes`.

## 8. The monthly Lambda and snapshots

A Go Lambda, `cmd/monthly-unpause`, runs at 06:00 UTC on the 1st of each month
(EventBridge Scheduler, `cron(0 6 1 * ? *)`). For every pause action on a `bedrock-*` budget in
the account:

1. It saves a snapshot of the month that just ended.
2. It re-arms each pause action that is paused (over the limit) or off: it reverses the action if
   needed, then resets it to armed.
3. It leaves pauses by hand alone. They last until an admin runs `unpause`.

The Lambda uses the `provided.al2023` runtime on arm64. It shares the pause, re-arm and snapshot
code with the CLI. It is built by `go generate` and embedded in the `bedrock-admin` binary with
`go:embed`, and `apply` creates or updates it (Step 1.4). `plan` compares the deployed code's
SHA-256 with the embedded one and shows an update when they differ, so upgrading the CLI and running
`apply` upgrades the Lambda. A binary built without `go generate` (for example by `go install`) has
no Lambda code, and Step 1.4 shows a conflict.

### Snapshots

AWS Budgets keeps no history of limits or pause states, so the Lambda saves one before it re-arms:
`s3://<bucket>/<snapshot_prefix>/YYYY-MM.json` in the cost export bucket (default prefix
`bedrock-admin/snapshots`). The Lambda's role may write only there; the bucket is in the same
account, so the bucket policy needs no change.

| Field | Content |
| --- | --- |
| `month`, `taken_at`, `account` | Which month, when, which account |
| `users[]` | `email`, `name`, `budget`, `role`, `limit_usd`, `pause_at_percent`, `trigger_usd`, `state`, and for a pause, `paused_at`, `paused_by` and `reason` |

`bedrock-admin usage --month YYYY-MM` uses the snapshot for past limits and pause states. Without
one, limits come from the file and pause state is unknown.

## 9. Usage and untracked spend

`bedrock-admin usage` reads spend from two places, and no Cost Explorer:

| Data | Source |
| --- | --- |
| This month's limits and pause states | AWS Budgets |
| Spend, tracked and untracked, this month and past months | The cost export's Parquet files |
| Past limits and pause states | The snapshots |

The forecast for this month is the spend so far, scaled to the whole month.

The CLI lists `s3://<bucket>/<prefix>/<export>/data/BILLING_PERIOD=YYYY-MM/*.parquet`, streams each
file, reads only four columns, and adds up the cost per caller. No query engine is needed.

| Column | Use |
| --- | --- |
| `line_item_iam_principal` | The caller ARN; rows without one are not model calls |
| `tags` | `iamPrincipal/owner`; a row without a known owner is untracked |
| `line_item_unblended_cost` | Cost |
| `line_item_usage_start_date` | Last used |

Untracked spend is spend that no budget counted. Each caller is classified by its ARN:

| Used via | Caller |
| --- | --- |
| `SSO role <permission set>` | An SSO session called Bedrock directly, outside the personal role |
| `personal role without owner tag: <role>` | A personal role whose calls had no owner tag, for example before Step 1.6 was active |
| `personal role without budget: <role>` | A personal role tagged with an owner who has no budget in the file |
| `role <role>` | Any other IAM role |
| `IAM user` | An IAM user, including long-term Bedrock API keys |

The "who" is the last part of the ARN: the SSO email for SSO sessions and personal roles. So
untracked spend is still attributed to a person, after the fact. The cost export lags up to about
16 hours.

## 10. The `id`: what `apply` may change

Everything `apply` creates is tagged `bedrock-admin:id` with the file's `id`. EventBridge schedules
can't be tagged, so for the schedule the id is in its description. `apply`, offboarding and
`uninstall` only change or delete resources with that id:

| Found | Result |
| --- | --- |
| Account resource with the same id | Managed: created, updated or removed as the file says |
| Account resource with another id | `ok (managed by <id>)`, left alone |
| Account resource without the tag | Conflict: made outside `bedrock-admin`; remove it, then apply |
| Personal role or budget with another id, or none | `not managed`, left alone |

So a file with a different `id`, such as an e2e run's `e2e-<run id>`, can't offboard real users or
remove another setup's resources. There is no adoption of untagged resources. `bedrock-admin export`
captures an existing account's users into a file, as a convenience.

## 11. Security model

| Who | Can use a personal role? |
| --- | --- |
| The user, signed in with SSO, session name = their email | Yes, their own role only |
| Another user, with any session name | No |
| An account admin (for example `AWSAdministratorAccess`) | No: the trust conditions apply to every caller |
| IAM users, other roles, or a personal role chaining into another | No: only SSO sessions are trusted |

The protection doesn't depend on admin rights, because `aws:userid` is a value Identity Center
sets.

| Control | Covers | Doesn't cover |
| --- | --- | --- |
| Trust policy | Who can use a personal role | Admins who edit trust policies or tags |
| `bedrock-invoke` policy | Personal roles: Claude only, Fable denied | Calls from other roles |
| Budget and pause | Spend through personal roles | Calls outside personal roles |
| Step 1.7 deny | Model calls from SSO roles except the admin set | IAM users, other roles, API keys, the admin set |
| `usage` untracked section | Every Bedrock caller in the cost export, after the fact | Nothing in real time |

- `bedrock-admin` needs only account-level admin rights. It never touches AWS Organizations.
- `bedrock-deny` and the budget actions role are scoped to `/bedrock-users/` roles.
- The cost export bucket is private and writable only by Data Exports.
- `bedrock.yaml` has no secrets.
- Personal roles can call only Bedrock. An agent such as Claude Code running with the personal role
  can't do anything else in AWS.

## 12. Behavior and limits

| Topic | Behavior |
| --- | --- |
| Billing delay | Budgets and the cost export run 3 to 16 hours behind usage |
| Pause delay | A soft limit: the pause comes 3 to 16 hours after spend crosses the trigger, so users can go somewhat over |
| Pause effect | Within seconds, including running sessions. Access returns within about a minute of an unpause. |
| Re-arming | A reversed action never fires again until it is reset. `apply` and the Lambda always do both. |
| Untracked spend | No budget, no alert, no pause, no team charge. Found after the fact by `usage`. |
| Credentials | Role chaining limits personal-role credentials to 1 hour. The AWS CLI, the SDKs and Claude Code refresh them while the SSO sign-in is valid. |
| Personal role scope | Bedrock only; other AWS commands need the SSO profile |
| Emails | Only AWS Budgets sends email (alerts and the automatic pause), with fixed wording. No email for a pause or unpause by hand. |
| Claude Code settings | The `/login` wizard writes `~/.claude/settings.json`, which every Claude Code session on the machine reads. Users with a personal Claude account should use a separate `CLAUDE_CONFIG_DIR`. |

## 13. Testing

Go unit tests cover the logic that doesn't need AWS: config validation, flags, the plan diff, the
Parquet reader (against the fixtures in `tests/fixtures/`), the caller classification and the
output formats. They run with `go test ./...`, and CI runs them with `go vet` and `golangci-lint` on
every pull request.

The Robot Framework suites in `tests/e2e/` cover only end-to-end behavior, with the real binaries
and a real AWS account:

| Suite | Covers |
| --- | --- |
| `cli` | A smoke test of the built binaries, with no AWS |
| `account` | The account steps: `plan`, `apply`, re-runs, `uninstall` |
| `users` | Onboarding, limit changes and offboarding |
| `user-access` | The user flow: `bedrock setup`, calls through the personal role, `bedrock doctor`, direct SSO calls denied, a pause seen by the user |
| `usage` | The report, against Parquet fixtures uploaded to the bucket |
| `billing` | Real spend, the automatic pause and the monthly Lambda, in phases over a day or more |

The e2e suites are run by hand with `tests/e2e/run.sh`, not in CI. Each run uses its own `id`, so it
can't touch real users. See [tests/e2e/README.md](tests/e2e/README.md).

Differences from the original design spec:

- The `cli` suite doesn't test each validation rule. Validation moved to the unit tests.
- `billing.robot` needs only the admin profile. Test users spend through their own personal role:
  a test-only trust statement lets the admin profile assume it with the session name `<email>`.
- In billing phase 2 the monthly Lambda re-arms both user a (paused) and user d (off), so phase 3
  expects a and d paused again. The check that "off stays off despite more spend" is not part of
  phase 3, since the mid-month Lambda run re-arms d.

## 14. Implementation and release

- Go, one module, standard layout: `cmd/bedrock-admin`, `cmd/bedrock`, `cmd/monthly-unpause` and
  `internal/`. One static binary per CLI, nothing else to install.
- Libraries: the AWS SDK for Go v2, `parquet-go` for the cost export, `go.yaml.in/yaml/v3`, `cobra`,
  and `text/tabwriter` for tables.
- CI (`.github/workflows/ci.yml`): `go generate`, `go vet`, `golangci-lint` and `go test -race`.
- Release (`.github/workflows/release.yml`): on a `v*` tag, GoReleaser builds both CLIs for macOS,
  Linux and Windows (amd64 and arm64), with the Lambda embedded in `bedrock-admin`, and attaches
  them and checksums to the GitHub release.

## 15. Later and open items

| Item | Notes |
| --- | --- |
| SES email | For what AWS Budgets can't send: pause and unpause by hand, an unpause by `apply`, a monthly spend report. Needs a verified sender domain and SES production access. |
| Data freshness in `usage` | Show how recent the cost export data is in the untracked section |
| IAM account access manager | Assigns the personal role directly, without role chaining. IT must enable it in the management account; only the trust policy and the user sign-in change. |
| Exception approvals | Decide who may approve an unpause or a limit raise mid-month, and how |
