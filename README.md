# Amazon Bedrock setup: per-person budgets and automatic pausing

Engineers use Claude and other models through **Amazon Bedrock** with their SSO login. Each engineer
has a **personal monthly budget**. When someone goes over it, **only that person is paused**, and
everyone is unpaused automatically on the 1st of the month.

## Which document do I need?

| You are | Read |
|---|---|
| An **engineer** who wants to use Bedrock or Claude Code | [User guide](bedrock-user-guide.md) |
| The **admin** of an AWS account: you set up the account and onboard engineers | [Admin guide](bedrock-admin-guide.md) |
| Reviewing **how and why** it works (IT, security, finance) | [Design and mechanism](bedrock-guideline.md) |

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
| [`bedrock-user-guide.md`](bedrock-user-guide.md) | Engineer setup, troubleshooting, what happens when you're paused |
| [`bedrock-admin-guide.md`](bedrock-admin-guide.md) | Part 1: one-time account setup. Part 2: onboarding and managing engineers |
| [`bedrock-guideline.md`](bedrock-guideline.md) | Design decisions, how the pieces work, test results, open items |
| [`scripts/bedrock-user.sh`](scripts/bedrock-user.sh) | Admin script: `onboard`, `status`, `set-limit`, `unpause`, `offboard` |
| [`scripts/untracked-usage.py`](scripts/untracked-usage.py) | Admin report: who used Bedrock without their personal role |
| [`scripts/monthly-unpause/`](scripts/monthly-unpause/) | Lambda that unpauses and re-arms everyone on the 1st |
| [`poc/`](poc/) | Proof-of-concept files from the original tests. Kept for reference. |

## Status

Tested end to end in account `111122223333` (2026-09-30 → 2026-10-05): personal roles, per-person
cost tags, a budget with the per-person filter, the 80% alert, the automatic pause, unpausing, and
the monthly re-arm. Open items are listed in
[bedrock-guideline.md §8](bedrock-guideline.md#8-open-items).

> Account IDs, names, emails, bucket names and resource IDs in this repository are **placeholders**
> (for example account `111122223333` and `alice@example.com`). Replace them with your own values.
