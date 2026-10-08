# Test fixtures

Synthetic CUR 2.0 cost export files and a monthly snapshot. The Go unit tests in `internal/usage`
and the e2e suite `tests/e2e/suites/usage.robot` both use them. All emails are `usage-*@example.test`
and the account is `111122223333`.

Regenerate with `go test ./internal/usage -run TestFixtures -update`. The rows are defined in
`internal/usage/usage_test.go`. Update the numbers below if you change them.

## `cur-this-month.parquet` (uploaded as this month)

| Caller (session) | Used via | Owner tag | USD | Days |
| --- | --- | --- | --- | --- |
| usage-a@example.test | personal role | usage-a@example.test | 13.25 (10.00 + 2.50 + 0.75) | 10-01, 10-02 |
| usage-b@example.test | personal role | usage-b@example.test | 4.00 | 10-03 |
| usage-c@example.test | personal role | usage-c@example.test | 1.20 | 10-03 |
| usage-ghost@example.test | personal role without budget: bedrock-user-usage-ghost | usage-ghost@example.test | 0.80 | 10-04 |
| usage-x@example.test | personal role without owner tag: bedrock-user-usage-x | (none) | 0.10 | 10-04 |
| usage-sso@example.test | SSO role PowerUser | (none) | 2.70 (2.30 + 0.40) | 10-02, 10-05 |
| usage-a@example.test | SSO role PowerUser | (none) | 0.60 | 10-05 |
| usage-ci-bot | IAM user | (none) | 3.20 | 10-04 |
| session1 | role app-role | (none) | 0.20 | 10-05 |
| usage-tiny | IAM user | (none) | 0.004 (left out of the list) | 10-05 |
| (no caller: S3 storage) | ignored | | 5.00 | |

With users usage-a, usage-b and usage-c having budgets (usage-ghost has none):

- Tracked total $18.45; untracked total $7.60, six callers listed (usage-tiny is under $0.01).
- `usage usage-a@example.test`: total $13.85. Days: 10-01 $10.00, 10-02 $3.25, 10-05 $0.60.
  Models: Claude Sonnet 4.5 $11.35, Claude Haiku 4.5 $2.50. Used via: personal role $13.25, SSO role PowerUser $0.60.
- `usage usage-sso@example.test`: not a user with a budget, total $2.70.

## `cur-last-month.parquet` (uploaded as last month) and `snapshot-last-month.json`

| Caller | Owner tag | USD |
| --- | --- | --- |
| usage-a@example.test | usage-a@example.test | 55.00 |
| usage-b@example.test | usage-b@example.test | 20.00 |
| usage-sso@example.test (SSO role PowerUser) | (none) | 1.00 |

Snapshot: usage-a limit $50, paused Sep 12 (110% used); usage-b limit $100, armed; usage-c limit $30,
paused by hand Sep 20 ("key leak"), $0 spent. Tracked $75.00, untracked $1.00. Upload the snapshot
as `<snapshot_prefix>/<last month>.json`; only its contents matter, not its `month` field.
