# Amazon Bedrock setup: per-person budgets and automatic pausing

Engineers use Claude and other models through **Amazon Bedrock** with their SSO login. Each engineer
has a **personal monthly budget**. When someone goes over it, **only that person is paused**, and
everyone is unpaused automatically on the 1st of the month.

## Which document do I need?

| You are | Read |
|---|---|
| An **engineer** who wants to use Bedrock or Claude Code | [User guide](user-guide.md) |
| The **admin** of an AWS account: you set up the account and onboard engineers | [Admin guide](admin-guide.md) |
| Reviewing **how and why** it works (IT, security, finance) | [Design and mechanism](design.md) |

## Getting started

Setup happens in this order. **Engineers cannot start until their admin has finished steps 1 and 2.**

| Step | Who | What | Guide | How often |
|---|---|---|---|---|
| 1 | Admin | Set up the AWS account: cost export, pause policy, Budgets role, monthly auto-unpause | [Admin guide, Part 1](admin-guide.md#part-1-one-time-account-setup) | Once per account, about 20 minutes |
| 2 | Admin | Onboard each engineer: personal role, budget and automatic pause. Send them their `role_arn` and `role_session_name` | [Admin guide, Part 2](admin-guide.md#onboard-an-engineer) | Once per engineer, about 1 minute |
| 3 | Engineer | Set up the AWS profile and Claude Code | [User guide](user-guide.md) | Once per computer, about 5 minutes |
| 4 | Admin | Check for Bedrock usage outside personal roles | [Admin guide, Part 2](admin-guide.md#find-who-isnt-using-their-personal-role) | Monthly |

Billing data runs about a day behind. An engineer's spend appears in their budget about a day after
their first Bedrock call, and the first cost export arrives within 24 hours of step 1.

## How it works

```
 Engineer (SSO login)
    │  aws login  →  profile "bedrock"
    ▼
 Personal role  bedrock-user-<name>      Bedrock-only, tagged owner=<sso-email>
    │  bedrock:InvokeModel
    ▼
 Amazon Bedrock ──► billing records the caller and the role's tags
                        │
                        ▼
                  AWS Budget  bedrock-<name>  (filter: iamPrincipal/owner = <sso-email>)
                    80%  → email
                    100% → attach the bedrock-deny policy to that person's role  (pause)
                        │
                        ▼
                  1st of the month: Lambda bedrock-monthly-unpause unpauses and re-arms everyone
```

## Repository layout

| Path | What it is |
|---|---|
| [`user-guide.md`](user-guide.md) | Engineer setup, troubleshooting, what happens when you're paused |
| [`admin-guide.md`](admin-guide.md) | Part 1: one-time account setup. Part 2: onboarding and managing engineers |
| [`design.md`](design.md) | Why it works this way: concepts, design decisions, each component and its policies, security model, limits, open items |
| [`scripts/bedrock-user.sh`](scripts/bedrock-user.sh) | Admin script: `onboard`, `status`, `set-limit`, `unpause`, `offboard` |
| [`scripts/untracked-usage.sh`](scripts/untracked-usage.sh) | Admin report: who used Bedrock without their personal role |
| [`scripts/monthly-unpause/`](scripts/monthly-unpause/) | Lambda that unpauses and re-arms everyone on the 1st |
| [`poc/`](poc/) | Proof-of-concept files from the original tests. Kept for reference. |

## Status

Tested end to end in account `111122223333` (2026-09-30 → 2026-10-05): personal roles, per-person
cost tags, a budget with the per-person filter, the 80% alert, the automatic pause, unpausing, and
the monthly re-arm. Open items are listed in
[design.md §8](design.md#8-open-items).

> Account IDs, names, emails, bucket names and resource IDs in this repository are **placeholders**
> (for example account `111122223333` and `achen@example.com`). Replace them with your own values.
