# Proof-of-concept files

These are the files used for the original tests in account `111122223333` (2026-09-30 → 2026-10-05).
They are kept for reference. **Use the guides and [`../scripts/`](../scripts/) for real setups.**

| File | Used for |
|---|---|
| `trust-alice.json`, `bedrock-invoke.json` | The first personal role, `bedrock-user-alice` |
| `bedrock-deny.json` | The shared pause policy |
| `budget-actions-trust.json`, `budget-actions-permissions.json` | The role AWS Budgets uses to pause people |
| `poc-budget.json`, `check-status.sh` | First budget-action test (budget deleted) |
| `invoke-as-alice.sh` | Bedrock call through the personal role |
| `unpause-me.sh` | Self-unpause for `bedrock-alice` while Claude Code itself was paused |
| `unpause/` | Monthly unpause Lambda test: IAM policies, throwaway test role, polling scripts. The current Lambda code is in [`../scripts/monthly-unpause/`](../scripts/monthly-unpause/). |
