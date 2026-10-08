# Amazon Bedrock: per-person budgets and automatic pausing

Engineers use Claude through Amazon Bedrock with their SSO login. Each engineer has a personal
role and a monthly budget. When someone reaches their limit, only that person is paused. Everyone is
re-armed on the 1st of the month.

Two command-line tools do the work:

- `bedrock-admin`, for the account admin. It sets up the account and manages users from one file,
  `bedrock.yaml`.
- `bedrock`, for engineers. It sets up the AWS profile of their personal role and checks it.

## Contents

- [Which document do I need?](#which-document-do-i-need)
- [Getting started](#getting-started)
- [How it works](#how-it-works)
- [Commands](#commands)
- [Installation](#installation)
- [Repository layout](#repository-layout)
- [Development](#development)

## Which document do I need?

| You are | Read |
| --- | --- |
| An engineer who wants to use Bedrock or Claude Code | [User guide](user-guide.md) |
| The admin of an AWS account: you set up the account and manage users | [Admin guide](admin-guide.md) |
| Reviewing how and why it works (IT, security, finance) | [Design](design.md) |
| Running the end-to-end tests | [tests/e2e/README.md](tests/e2e/README.md) |

## Getting started

Engineers can't start until the admin has run steps 1 to 3.

| Step | Who | What | Guide |
| --- | --- | --- | --- |
| 1 | Admin | Install `bedrock-admin`, sign in, run `bedrock-admin doctor` | [Admin guide, step 1](admin-guide.md#1-install-and-check-your-environment) |
| 2 | Admin | Write `bedrock.yaml` with `bedrock-admin configure` | [Admin guide, step 2](admin-guide.md#2-create-bedrockyaml) |
| 3 | Admin | `bedrock-admin plan`, then `bedrock-admin apply`: account setup (Steps 1.1–1.7) and users | [Admin guide, step 3](admin-guide.md#3-set-up-the-account-plan-then-apply) |
| 4 | Admin | Onboard users: add them to `users:` in `bedrock.yaml`, then `apply`, and send each one the command `apply` prints | [Admin guide, step 4](admin-guide.md#4-day-to-day-changes-are-file-edits) |
| 5 | Engineer | `aws sso login`, `bedrock setup`, then `bedrock claude` (tests Claude Code, prints its settings) | [User guide](user-guide.md) |
| 6 | Admin | Monthly: `bedrock-admin usage` to see spend and calls outside personal roles | [Admin guide, step 7](admin-guide.md#7-read-spend) |

Billing data runs behind. The first cost export arrives within 24 hours of the first `apply`, and
an engineer's spend shows up in their budget 3 to 16 hours after the calls.

## How it works

```
 Engineer (SSO login)
    │  aws sso login, then profile "bedrock" (written by bedrock setup)
    ▼
 Personal role  bedrock-user-<name>      Claude models only, tagged owner=<sso-email>
    │  bedrock:InvokeModel
    ▼
 Amazon Bedrock ──► billing records the caller and the role's tags
                        │
                        ▼
                  AWS Budget  bedrock-<name>  (filter: iamPrincipal/owner = <sso-email>)
                    alert_at_percent  → email
                    pause_at_percent  → attach bedrock-deny to that person's role (paused)
                        │
                        ▼
                  1st of the month, 06:00 UTC: Lambda bedrock-monthly-unpause
                  saves a snapshot, then re-arms every pause action
```

`bedrock-admin apply` creates all of this from `bedrock.yaml`, including the Lambda, which is built
into the `bedrock-admin` binary.

## Commands

`bedrock-admin`, for the account admin:

| Command | What it does |
| --- | --- |
| `configure` | Writes the settings part of `bedrock.yaml`, by asking or from flags |
| `doctor` | Checks the admin's credentials, permissions, account setup, cost export data and models |
| `plan` | Shows what `apply` would change. Read-only; exits 2 when there are changes |
| `apply` | Makes the account and the users match `bedrock.yaml`. Removals need `--yes` |
| `export` | Prints the current account, including its users, as a `bedrock.yaml` |
| `pause` | Pauses a user by hand, until `unpause`. The monthly Lambda leaves it in place |
| `unpause` | Restores a paused user's access |
| `usage` | Spend per user, and spend outside personal roles. Exits 2 when it finds untracked spend |
| `uninstall` | Removes the account setup. Refuses while users remain |

`bedrock`, for engineers:

| Command | What it does |
| --- | --- |
| `setup` | Writes the `bedrock` profile for your personal role in `~/.aws/config`, then runs `doctor` |
| `doctor` | Checks your SSO sign-in, the `bedrock` profile, your personal role and a model call |
| `claude` | Tests Claude Code with the `bedrock` profile and prints the settings to use. Changes no Claude settings |

Both CLIs exit with 0 when everything is fine, 1 on an error, and 2 when they ran but found a
problem. Run `<command> --help` for the flags. The [admin guide](admin-guide.md#commands) and
[user guide](user-guide.md#commands) link each command to its section.

## Installation

Each release on GitHub has archives of both CLIs for macOS, Linux and Windows, on amd64 and arm64,
built by GoReleaser:

| File | Contents |
| --- | --- |
| `bedrock-admin_<version>_<os>_<arch>.tar.gz` | `bedrock-admin` (`.zip` on Windows) |
| `bedrock_<version>_<os>_<arch>.tar.gz` | `bedrock` (`.zip` on Windows) |
| `checksums.txt` | SHA-256 of every archive |

`<os>` is `darwin`, `linux` or `windows`, and `<arch>` is `amd64` or `arm64`. Unpack the archive
and put the binary on your `PATH`.

With Go installed, you can also build from source:

```bash
go install github.com/SUSE/high-impact-ai-initiative/cmd/bedrock@latest
go install github.com/SUSE/high-impact-ai-initiative/cmd/bedrock-admin@latest
```

A `bedrock-admin` built by `go install` has no monthly Lambda inside, because the Lambda package is
generated at build time and not committed. `plan` then reports Step 1.4 as a conflict ("this
bedrock-admin build has no Lambda code"). Use a release binary, or clone the repository and run
`make build`.

Check the install with `bedrock-admin --version` and `bedrock --version`.

## Repository layout

| Path | What it is |
| --- | --- |
| [`user-guide.md`](user-guide.md) | Engineer setup, troubleshooting, what happens when you're paused |
| [`admin-guide.md`](admin-guide.md) | Account setup, managing users, pauses, spend, cheat sheet |
| [`design.md`](design.md) | How and why it works: the model, each component, security, limits, testing, open items |
| `cmd/bedrock-admin/` | Admin CLI |
| `cmd/bedrock/` | User CLI |
| `cmd/monthly-unpause/` | The monthly Lambda (`provided.al2023`), embedded in `bedrock-admin` |
| `internal/` | Shared packages: config, plan and apply, account steps, users, pause, usage (Parquet reader), the user CLI |
| `tests/e2e/` | Robot Framework suites that run the real binaries against a real AWS account, by hand. See [tests/e2e/README.md](tests/e2e/README.md). |
| `tests/fixtures/` | Synthetic cost export Parquet files and a snapshot, used by the unit tests and the `usage` suite |
| `poc/` | Proof-of-concept files from the original tests. Kept for reference only. |
| `.github/workflows/` | `ci.yml` (vet, lint, unit tests) and `release.yml` (GoReleaser on a `v*` tag) |

## Development

You need Go (the version in `go.mod`) and, for linting, `golangci-lint`.

| Command | Does |
| --- | --- |
| `make` | `generate`, `vet`, `lint`, `test` and `build` |
| `make generate` | `go generate ./...`: builds the monthly Lambda package that `bedrock-admin` embeds |
| `make build` | Builds `bin/bedrock-admin` and `bin/bedrock` |
| `make test` | `go test ./...` |
| `make vet` | `go vet ./...` |
| `make lint` | `golangci-lint run ./...` |
| `make e2e SUITE=<suite>` | `tests/e2e/run.sh <suite>` |
| `make clean` | Removes `bin/`, `dist/` and the generated Lambda package |

Run `make generate` once before `go vet`, `go test` or `go build` in a fresh clone. Without it,
the build works, but `bedrock-admin` has no Lambda to deploy.

CI (`.github/workflows/ci.yml`) runs `go generate`, `go vet`, `golangci-lint` and
`go test -race ./...` on pull requests and on `main`. It does not run the end-to-end tests. Run
them by hand with `tests/e2e/run.sh`, as described in [tests/e2e/README.md](tests/e2e/README.md).
Every suite except `cli` uses a real AWS account.

To release, push a `v*` tag. `.github/workflows/release.yml` runs GoReleaser
(`.goreleaser.yaml`), which builds both CLIs and attaches the archives and checksums to the GitHub
release.

> Account IDs, names, emails, bucket names and resource IDs in this repository are placeholders
> (for example account `111122223333` and `achen@example.com`). Replace them with your own values.
