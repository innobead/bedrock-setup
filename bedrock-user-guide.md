# Amazon Bedrock: User Guide

> **Version 1.0 (2026-10-05).** For engineers who use Claude and other models through Amazon
> Bedrock. Setup takes about 5 minutes.

> **Before you begin:** your admin must first set up the AWS account and onboard you. When that is
> done, they send you your `role_arn` and `role_session_name`. If you haven't received them, ask
> your admin. Until then, the setup below fails with `AccessDenied`.

## How it works

- You use Bedrock through a **personal AWS role** that only you can use, from your SSO login.
- You have a **monthly budget**. You get an email at 80%. At 100% your Bedrock access is
  **paused** until your admin unpauses you, or until the 1st of next month.

## Why use your personal role

You *can* call Bedrock from other profiles, such as the `AWSAdministratorAccess` SSO login, but please don't. Those
calls are **not anonymous**: billing still records your SSO email, and your admin sees them in
a monthly report. What you lose is everything else:

| | Personal role (`bedrock` profile) | Any other profile |
|---|---|---|
| Who made the call | Yes, your email | Yes, your email (found later in a report) |
| Counts toward your budget | Yes | No |
| Email at 80% of your budget | Yes | No |
| Automatic pause at 100% | Yes | No, so there is no limit at all |
| Cost charged to your team/product | Yes | No, it shows as unallocated cost |
| Shows up in Cost Explorer per person | Yes, the next day | No, only in the monthly report |
| What a coding agent (e.g. Claude Code) can do in AWS | Only call Bedrock | Everything that profile allows. With `AWSAdministratorAccess`, that's the whole account. |

So the cost still comes back to you, but without the warning, the limit and the team allocation
that protect you and your team. Using Bedrock from another profile is followed up by your admin.

## Before you start

- You can sign in to the AWS account with SSO.
- Your admin has onboarded you and sent you your **`role_arn`** and **`role_session_name`**.
- Your admin has your **real mailbox address** for budget emails. Your SSO login email may not
  receive mail.
- You have the AWS CLI v2 installed.

## Setup

### Step 1 Sign in with SSO

```bash
aws login                         # or: aws sso login --profile <your-sso-profile>
aws sts get-caller-identity       # the ARN must end in /<your-sso-email>
```

> ⚠️ `aws login` reuses whatever identity your browser console is signed in as. Check the output.

### Step 2 Add your Bedrock profile

Add this to `~/.aws/config` and replace **every** `<…>` placeholder:

```ini
[profile bedrock]
role_arn = arn:aws:iam::<ACCOUNT_ID>:role/bedrock-users/bedrock-user-<name>
source_profile = default
role_session_name = <your-sso-email>
region = us-west-2
```

- `source_profile` is the profile you signed in with in Step 1 (`default` if you used `aws login`).
- `role_session_name` must be **exactly** your SSO email.
- ⚠️ Don't put comments at the end of a line. Put them on their own line, starting with `#`.

### Step 3 Check it

```bash
aws sts get-caller-identity --profile bedrock
# Expect: arn:aws:sts::<ACCOUNT_ID>:assumed-role/bedrock-user-<name>/<your-sso-email>
```

### Step 4 Set up Claude Code

Start Claude Code and sign in to Bedrock with the built-in wizard. No config file to edit:

1. Run `claude` and type `/login`. (`/setup-bedrock` opens the same wizard.)
2. Choose **3rd-party platform → Amazon Bedrock**.
3. When asked how to authenticate to AWS, pick the **AWS profile** `bedrock` from Step 2.
   ⚠️ Not `default` or your SSO profile: only `bedrock` counts toward your budget.
4. Choose region **us-west-2** and accept the suggested models.

Check it with `/status`: it should show Amazon Bedrock. A test call should then appear under your
role `bedrock-user-<name>` (your admin can confirm).

- Your Bedrock role can **only** call Bedrock. For any other AWS command, add `--profile default`.
- Credentials renew automatically. When your SSO sign-in expires, run `aws login` again.
- To change the profile or region later, run `/login` again. (`/logout` does not apply to Bedrock.)

> ⚠️ **Also use a personal Claude account?** The wizard saves to `~/.claude/settings.json`, which
> **every** Claude Code session on your machine reads, including the Claude Mac app's Code tab. All
> of them would then bill to the company. Keep work in its own config folder instead:
>
> ```bash
> # Add to ~/.zshrc, open a new terminal, then run claude-work and do steps 1–4 there
> alias claude-work='CLAUDE_CONFIG_DIR=~/.claude-work claude'
> ```
>
> `claude-work` then uses Bedrock, and plain `claude` and the Mac app keep your personal login.

### If something goes wrong

| You see | Fix |
|---|---|
| `AccessDenied … sts:AssumeRole` | A `<…>` placeholder is still in `role_arn`, or `role_session_name` isn't exactly your SSO email |
| `The config profile (default  # …) could not be found` | Move the end-of-line comment in `~/.aws/config` to its own line |
| Claude Code uses the wrong identity or account | Run `/login` again and pick the `bedrock` profile. Check with `/status`. |
| `Token has expired` / SSO session expired | Run `aws login` again |
| `AccessDeniedException … bedrock:InvokeModel` (it worked before) | You're paused. See [When you're paused](#when-youre-paused). |
| `AccessDenied` on a non-Bedrock command | Expected. Use `--profile default`. |

---

## When you're paused

Bedrock calls fail with `AccessDeniedException … bedrock:InvokeModel … explicit deny`, and you get
an email from AWS Budgets.

- Ask your admin to raise your limit and unpause you. Access returns about 20 seconds later.
- Otherwise you are unpaused automatically on the 1st of next month.
- Don't switch to another profile (such as `AWSAdministratorAccess`) to keep working. That usage isn't
  counted against your budget, so it can't be tracked.
