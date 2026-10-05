#!/bin/bash
# List who used Bedrock models without their personal role, so their cost has no owner tag.
#
#   untracked-usage.sh <s3-export-prefix> [YYYY-MM]
#
#   s3-export-prefix  the CUR 2.0 export from Step 1.1, up to and including the export name,
#                     e.g. s3://my-billing-bucket/cur/my-export
#   YYYY-MM           billing month, default: this month (UTC)
#
# Needs the AWS CLI (run with your admin profile) and the DuckDB CLI:
#   macOS: brew install duckdb     Linux: see https://duckdb.org/docs/installation
set -euo pipefail

usage() { sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
die() { echo "error: $*" >&2; exit 1; }

[ $# -ge 1 ] && [ $# -le 2 ] && [[ "$1" == s3://* ]] || usage
command -v duckdb >/dev/null || die "DuckDB CLI is missing. Install it with: brew install duckdb"
prefix=${1%/}
month=${2:-$(date -u +%Y-%m)}
[[ "$month" =~ ^[0-9]{4}-[0-9]{2}$ ]] || die "month must be YYYY-MM: $month"
src="$prefix/data/BILLING_PERIOD=$month/"

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
aws s3 cp --recursive --quiet "$src" "$tmp" --exclude '*' --include '*.parquet'
ls "$tmp"/*.parquet >/dev/null 2>&1 || die "no export files found under $src"

calls="SELECT line_item_iam_principal AS arn,
         coalesce(tags['iamPrincipal/owner'], '') AS owner,
         line_item_unblended_cost AS cost,
         line_item_usage_start_date AS ts
  FROM read_parquet('$tmp/*.parquet')
  WHERE coalesce(line_item_iam_principal, '') <> ''"

duckdb -noheader -list -c "
WITH calls AS ($calls)
SELECT printf('Bedrock model spend in $month: tracked \$%.2f, untracked \$%.2f',
              coalesce(sum(cost) FILTER (WHERE owner <> ''), 0),
              coalesce(sum(cost) FILTER (WHERE owner = ''), 0)) || chr(10)
FROM calls;"

report=$(duckdb -noheader -list -c "
WITH calls AS ($calls)
SELECT printf('%-30s %-52s %9.2f  %s', who, used_via, usd, last_used)
FROM (
  SELECT
    split_part(arn, '/', -1) AS who,  -- SSO email or IAM user name
    CASE WHEN arn LIKE '%:assumed-role/AWSReservedSSO_%'
           THEN 'SSO role ' || regexp_extract(split_part(arn, '/', 2), 'AWSReservedSSO_(.*)_[0-9a-f]+\$', 1)
         WHEN arn LIKE '%:assumed-role/bedrock-user-%'
           THEN 'personal role without owner tag: ' || split_part(arn, '/', 2)
         WHEN arn LIKE '%:assumed-role/%' THEN 'role ' || split_part(arn, '/', 2)
         WHEN arn LIKE '%:user/%' THEN 'IAM user'
         ELSE arn END AS used_via,
    sum(cost) AS usd,
    strftime(max(ts), '%Y-%m-%d') AS last_used
  FROM calls
  WHERE owner = ''
  GROUP BY ALL
  HAVING sum(cost) >= 0.01
)
ORDER BY usd DESC;")

if [ -z "$report" ]; then
  echo "Everyone used their personal role. Nothing to follow up."
else
  printf '%-30s %-52s %9s  %s\n' WHO 'USED VIA' USD 'LAST USED'
  echo "$report"
fi
