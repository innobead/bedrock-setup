# Amazon Bedrock: user guide

For engineers who use Claude through Amazon Bedrock, for example in Claude Code. Setup takes a few
minutes.

> Before you begin, your admin must have set up the AWS account and onboarded you. They send you
> the command to run, usually just `bedrock setup`. Until you are onboarded, setup fails at the
> `permission-set access` check.

## How it works

- You use Bedrock through a personal AWS role that only you can use, from your SSO login.
- You can use Anthropic Claude models, except the Claude Fable family, which is not approved.
- You have a monthly budget. You get an email from AWS Budgets when you pass the alert threshold
  (usually 80%). When you reach your limit, your Bedrock access is paused until your admin unpauses
  you, or until the 1st of next month.

## Why use your personal role

You can call Bedrock from other profiles, such as an `AWSAdministratorAccess` SSO login, but
please don't. Those calls are not anonymous: billing still records your SSO email, and your admin
sees them in the monthly `usage` report. What you lose is everything else:

| | Personal role (`bedrock` profile) | Any other profile |
| --- | --- | --- |
| Who made the call | Your email | Your email (found later in a report) |
| Counts toward your budget | Yes | No |
| Alert email before the limit | Yes | No |
| Pause at the limit | Yes | No, so there is no limit at all |
| Cost charged to your team or product | Yes | No, it shows as untracked spend |
| What a coding agent such as Claude Code can do in AWS | Only call Claude models | Everything that profile allows |

Your admin may also block model calls from other SSO profiles. Then the personal role is the only
way in.

## Before you start

- You can sign in to the AWS account with SSO, and you have an SSO profile in `~/.aws/config`
  (from `aws configure sso`), or you sign in with `aws login`.
- The AWS CLI v2 is installed.
- The `bedrock` CLI is installed. Download it for your platform from the GitHub release (see
  [Installation](README.md#installation)), or run
  `go install github.com/SUSE/high-impact-ai-initiative/cmd/bedrock@latest`.
- Your admin has onboarded you. If your SSO email doesn't receive mail, give your admin your real
  mailbox address for the budget emails.

## Setup

### Step 1. Sign in with SSO

```bash
aws sso login --profile <your-sso-profile>     # or: aws login
```

`aws login` reuses whatever identity your browser console is signed in as. `bedrock setup` checks
that it is an SSO login.

### Step 2. Write your Bedrock profile

Run the command your admin sent you. Usually it is:

```bash
bedrock setup
```

`setup` writes a `[profile bedrock]` block in `~/.aws/config` (or `$AWS_CONFIG_FILE`):

```ini
[profile bedrock]
role_arn = arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-achen
source_profile = my-sso
role_session_name = achen@example.com
region = us-west-2
```

- It reads your SSO identity to find the account and your SSO email, and derives your personal role
  from the email.
- `role_session_name` is set to your exact SSO email. The role only trusts that session name.
- The region comes from your SSO profile. Your admin adds `--region` to the command when Bedrock
  runs in another region, and `--role-arn` when your role has a custom name.
- It keeps the previous file as `config.bak`. If a `bedrock` profile already exists, it shows the
  old and new block and asks before replacing it. `--yes` replaces it without asking.
- If you have several SSO profiles, it asks which one you sign in with, or pass
  `--sso-profile <profile>`.
- `--profile-name` writes a profile with another name.

Then it runs `bedrock doctor`.

### Step 3. Check it

```bash
bedrock doctor
```

```
ok    profile: bedrock in /Users/achen/.aws/config
ok    SSO sign-in: achen@example.com (profile my-sso)
ok    permission-set access: may use arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-achen
ok    personal role: arn:aws:sts::111122223333:assumed-role/bedrock-user-achen/achen@example.com
ok    model us.anthropic.claude-opus-5-5: test call works
ok    model us.anthropic.claude-sonnet-5-5: test call works
ok    model us.anthropic.claude-haiku-5-5: test call works

All checks passed.
```

By default it tests Claude Opus, Sonnet and Haiku 5.5 (`us.`/`eu.` inference profiles, `global.` elsewhere). Test
another model with `--model <id>` (repeatable). `--json` prints the checks as JSON. It exits 2 when
a check fails, and prints the fix under it. See [If something goes wrong](#if-something-goes-wrong).

### Step 4. Set up Claude Code

`bedrock claude` checks that Claude Code works with the `bedrock` profile, then prints the settings
to add. It never writes your Claude Code settings: you add them yourself.

```text
$ bedrock claude
Testing Claude Code with the bedrock profile (one short call; your Claude Code settings are not used)...

ok    Claude Code with us.anthropic.claude-sonnet-5-5: answered OK

Claude Code settings for the bedrock profile. bedrock doesn't write them; add them yourself:

  {
    "awsAuthRefresh": "aws sso login --profile sso",
    "env": {
      "ANTHROPIC_DEFAULT_HAIKU_MODEL": "us.anthropic.claude-haiku-5-5",
      "ANTHROPIC_DEFAULT_OPUS_MODEL": "us.anthropic.claude-opus-5-5",
      "ANTHROPIC_DEFAULT_SONNET_MODEL": "us.anthropic.claude-sonnet-5-5",
      "ANTHROPIC_MODEL": "us.anthropic.claude-sonnet-5-5",
      "AWS_PROFILE": "bedrock",
      "AWS_REGION": "us-west-2",
      "CLAUDE_CODE_USE_BEDROCK": "1"
    }
  }

Where to put them, either way:
  - Work only: merge them into ~/.claude/settings.json. Every Claude Code session on this machine
    reads that file, so all of them then bill to your budget.
  - Next to a personal Claude account: put them in ~/.claude-work/settings.json and start Claude
    Code for work with: CLAUDE_CONFIG_DIR=~/.claude-work claude (for example as an alias in ~/.zshrc).

Or run claude, type /setup-bedrock (or /login, then 3rd-party platform, Amazon Bedrock), pick the
AWS profile bedrock and region us-west-2: the wizard writes the same env block.

Check in Claude Code with /status: it shows Amazon Bedrock and the bedrock profile.
us.anthropic.claude-opus-5-5 costs much more than Sonnet: switch to it with /model only when you need it.

Claude desktop app (Settings, Amazon Bedrock):
  AWS region:        us-west-2
  AWS profile name:  bedrock
  AWS CLI path:      /opt/homebrew/bin/aws
  Model list:        us.anthropic.claude-sonnet-5-5  (the first is the default)
                     us.anthropic.claude-opus-5-5
                     us.anthropic.claude-haiku-5-5
```

The test runs `claude -p` once in a temporary, empty config folder (`CLAUDE_CONFIG_DIR`) with no
tools, MCP servers, hooks or plugins, and removes it afterwards, so your own Claude Code settings and
login are untouched. When it fails, it prints the fix and exits 2; the settings are printed anyway.
Without `claude` on your PATH, or with `--no-test`, it only prints the settings. The last block
holds the values to enter in the Claude desktop app's Amazon Bedrock settings; the AWS CLI path line
appears when `aws` is on your PATH (the app may not see your shell's PATH). `--model` picks the
test model.

The model IDs are the standard defaults (Claude Opus, Sonnet and Haiku 5.5). Your settings are yours
to keep up to date: if your admin announces other models, change the model IDs in your Claude Code
settings and the desktop app's model list, and check one with `bedrock doctor --model <id>`.

- Your personal role can only call Claude models. For any other AWS command, use your SSO profile.
- Credentials renew automatically while your SSO sign-in is valid.
- To change the profile or region later, edit the settings or run `/setup-bedrock` again.

## If something goes wrong

Run `bedrock doctor`. It stops at the first failed setup check (the model checks all run) and prints
the fix under it:

| Failed check | Message | Fix |
| --- | --- | --- |
| `profile` | `no profile bedrock in <file>` | `bedrock setup` |
| `profile` | `missing role_arn, ...` | `bedrock setup --yes` |
| `profile` | `<key> has a comment at the end of the line` | Put comments on their own line, or run `bedrock setup --yes` |
| `SSO sign-in` | Your SSO session expired or you are not signed in | `aws sso login --profile <profile>` (or `aws login`) |
| `SSO sign-in` | `signed in as ..., not with SSO` | Set `source_profile` to your SSO profile, or run `bedrock setup --sso-profile <profile>` |
| `session name` | `role_session_name is ..., but it must be exactly your SSO email ...` | `bedrock setup --yes` |
| `permission-set access` | `cannot use <role ARN>: ... AccessDenied` | Ask your admin to onboard you (and check `role_arn`, if they gave you one), or to let your SSO permission set use the `bedrock-users` roles |
| `personal role` | The profile can't get credentials | `bedrock setup --yes` |
| `model <id>` | `AccessDeniedException`, on a Claude model | Your Bedrock access is paused (monthly budget used up). It comes back on the 1st, or ask your admin to unpause you. See [When you're paused](#when-youre-paused). |
| `model <id>` | `AccessDeniedException`, on another model | Your role may only call Anthropic Claude models (not Fable). Ask your admin if you need this one. |
| `model <id>` | `ValidationException` or not found | The model is not offered in your region. Use another model, or ask your admin for the right region. |

`bedrock setup` itself can stop with:

| Message | Fix |
| --- | --- |
| `no SSO profile in <file>` | Run `aws configure sso` (or `aws login`) first, or pass `--sso-profile` |
| `several SSO profiles in <file> (...)` | Pass `--sso-profile <profile>` (it asks when run in a terminal) |
| `profile <name> has no region` | Pass `--region`. Your admin knows the Bedrock region. |
| `reading your SSO identity ...` | Sign in first, with the command it prints |
| `the profile exists: run again with --yes to replace it` | Run `bedrock setup --yes` |

After your admin removes you, `doctor` stops at your role:

```text
$ bedrock doctor
ok    profile: bedrock in /Users/achen/.aws/config
ok    SSO sign-in: achen@example.com (profile my-sso)
FAIL  permission-set access: cannot use arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-achen: AccessDenied: User: arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_Dev_abc123/achen@example.com is not authorized to perform: sts:AssumeRole on resource: arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-achen
      fix: ask your admin to onboard achen@example.com (and check role_arn, if they gave you one), or to let your SSO permission set use the bedrock-users roles
```

In Claude Code, if it uses the wrong identity or account, run `/login` again, pick the `bedrock`
profile and check with `/status`.

## When you're paused

When you reach your limit, AWS Budgets attaches a deny policy to your personal role. Bedrock calls
fail with `AccessDeniedException`, and you get an email from AWS Budgets. `bedrock doctor` reports
it on the model checks:

```text
$ bedrock doctor
ok    profile: bedrock in /Users/achen/.aws/config
ok    SSO sign-in: achen@example.com (profile my-sso)
ok    permission-set access: may use arn:aws:iam::111122223333:role/bedrock-users/bedrock-user-achen
ok    personal role: arn:aws:sts::111122223333:assumed-role/bedrock-user-achen/achen@example.com
FAIL  model us.anthropic.claude-opus-5-5: AccessDeniedException
      fix: your Bedrock access is paused (monthly budget used up, or paused by your admin): ask your admin, or wait for the 1st
FAIL  model us.anthropic.claude-sonnet-5-5: AccessDeniedException
      fix: your Bedrock access is paused (monthly budget used up, or paused by your admin): ask your admin, or wait for the 1st
FAIL  model us.anthropic.claude-haiku-5-5: AccessDeniedException
      fix: your Bedrock access is paused (monthly budget used up, or paused by your admin): ask your admin, or wait for the 1st
```

The pause can come a few hours after you cross the limit, because billing runs behind.

- Your access comes back by itself on the 1st of next month.
- If you need it sooner, ask your admin. They can raise your limit, or unpause you for the rest of
  the month. Access returns within about a minute.
- Your admin can also pause you by hand, for example when a script runs away. That pause stays until
  they remove it, also past the 1st.
- Don't switch to another profile to keep working. That spend isn't counted against your budget,
  shows up in your admin's report, and may be blocked.
