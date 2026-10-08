# End-to-end tests

Robot Framework suites that run the real `bedrock-admin` and `bedrock` binaries (built for Linux)
against a real AWS account, in a container. They cover only what needs real binaries and real
AWS. Config validation, flags, the `plan` diff, the Parquet reader and report formatting are
covered by the Go unit tests (`go test ./...`).

The suites are run by hand, not in CI. Run the fast suites before merging a change that touches AWS
behavior, and `user-access` and `billing` before a release.

## Running

You need Docker (or `E2E_RUNTIME=podman`). Go is optional: without it, the CLIs are built in a
`golang` container.

```bash
cd tests/e2e
./run.sh cli                 # no AWS: smoke test of the built binaries
cp config/example.py config/$USER.py   # then fill it in
aws sso login --profile <ADMIN_PROFILE>
./run.sh account             # or users, user-access, usage
./run.sh fast                # every suite but billing
./run.sh users --test "Pause*"     # any robot option works after the suite
./run.sh shell               # a shell in the container
```

Reports go to `results/<suite>-<UTC time>/report.html`, and `results/latest` points to the newest.
`~/.aws` is mounted, so the container shares your SSO sign-in. The user CLI writes to a copy of your AWS config in the run's
directory, never to `~/.aws/config`.

## Suites

| Suite | Needs | Covers |
| --- | --- | --- |
| `cli` | nothing | The built binaries run. `configure` writes a file that `plan` loads, and plan then stops at the missing credentials. `bedrock doctor` and `bedrock setup` work without a profile. |
| `account` | admin | `apply` creates Steps 1.1–1.7 and a second `apply` changes nothing. `plan` names exactly the missing or drifted piece and `apply` fixes it. Step 1.7 handoff. `doctor`. `export` round trip. The `id` checks: resources with another id, or none, are left alone. `uninstall`. |
| `users` | admin | Onboarding (role, budget, alerts, pause action), changes in place, and drift. Removing a user needs `--yes`, and `--no-delete` skips it. Pause and unpause by hand. `apply` and the monthly Lambda leave a manual pause in place. Off for the month. The Lambda re-arms users and saves the snapshot. |
| `user-access` | admin, SSO | `bedrock setup` and its backup, calls through the personal role, `bedrock doctor`, direct SSO calls denied (Step 1.7), and a manual pause seen from the user's side. |
| `usage` | admin | `usage` on fixture cost export files and a snapshot uploaded under `e2e/<run>/`: totals, tracked and untracked spend, a past month from its snapshot, `--months`, and one person. |
| `billing` | admin | Everything that waits for AWS billing, in phases (below). |

## Safety

- Each run uses its own config with `id: e2e-<run id>` and users `e2e-<run id>-*`. `apply` deletes
  only resources tagged with its id, so a run can't offboard real users. Cleanup applies the run's
  config with `users: []`.
- Use a dedicated test account anyway. The `account` suite changes shared account resources
  (under `ACCOUNT_ID`). The monthly Lambda acts on every user in the account, so the library
  invokes it only when the Lambda is managed by `ACCOUNT_ID` and no personal roles except test
  users exist. `uninstall --delete-data` runs only with `ALLOW_DELETE_DATA = True`.
- Every suite except `cli` creates real AWS resources, and `user-access` and `billing` make paid
  model calls. A full `billing` run costs about $4.

## `billing`: phased

AWS Budgets updates spend about three times a day, and the cost export lags up to 24 h. So the
suite runs in phases that hand over through `results/billing/<run id>.json`:

```bash
./run.sh billing --include phase1    # any time: 4 users with a $1 limit; a, b, d spend $1.20, c $0.10
./run.sh billing --include phase2    # 12+ h later: a, b, d paused; unpause d (off); b's limit and
                                     # pause_at_percent; the monthly Lambda re-arms a and d
./run.sh billing --include phase3    # 24+ h after phase 1: a and d paused again; usage matches
                                     # the spend; untracked caller listed; cleanup
./run.sh billing --include manual    # after phase 2, with NOTIFY_EMAIL: asks whether the email arrived
```

Too early, or billing not caught up yet: the tests are skipped with "retry after <time>". 48 h after
phase 1 with no result, they fail. Later phases pick the newest unfinished run, and
`--variable RUN_ID:<id>` picks another.

Test users spend through their own personal role. A test-only trust statement lets the admin
profile assume the role with session name `<email>`, so the spend is attributed exactly as for a
real user. Without it, only the runner's own SSO login could assume a role.
