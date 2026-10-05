#!/usr/bin/env python3
"""List who used Bedrock models without their personal role, so their cost has no owner tag.

    untracked-usage.py <s3-export-prefix> [YYYY-MM]

    s3-export-prefix  the CUR 2.0 export from Step 1.1, up to and including the export name,
                      e.g. s3://my-billing-bucket/cur/my-export
    YYYY-MM           billing month, default: this month (UTC)

Needs the AWS CLI (run with your admin profile) and DuckDB: pip install duckdb
"""
import datetime
import subprocess
import sys
import tempfile

try:
    import duckdb
except ImportError:
    sys.exit("error: DuckDB is missing. Install it with: pip install duckdb")

QUERY = """
WITH calls AS (
  SELECT
    line_item_iam_principal                                   AS arn,
    tags['iamPrincipal/owner']                                AS owner,
    line_item_unblended_cost                                  AS cost,
    line_item_usage_start_date                                AS ts
  FROM read_parquet('{files}')
  WHERE coalesce(line_item_iam_principal, '') <> ''
)
SELECT
  split_part(arn, '/', -1)                                    AS who,  -- SSO email or IAM user name
  CASE WHEN arn LIKE '%:assumed-role/AWSReservedSSO_%'
         THEN 'SSO role ' || regexp_extract(split_part(arn, '/', 2), 'AWSReservedSSO_(.*)_[0-9a-f]+$', 1)
       WHEN arn LIKE '%:assumed-role/bedrock-user-%'
         THEN 'personal role without owner tag: ' || split_part(arn, '/', 2)
       WHEN arn LIKE '%:assumed-role/%' THEN 'role ' || split_part(arn, '/', 2)
       WHEN arn LIKE '%:user/%' THEN 'IAM user'
       ELSE arn END                                           AS used_via,
  round(sum(cost), 2)                                         AS usd,
  strftime(max(ts), '%Y-%m-%d')                               AS last_used
FROM calls
WHERE owner IS NULL OR owner = ''
GROUP BY ALL
HAVING sum(cost) >= 0.01
ORDER BY usd DESC
"""

TOTALS = """
SELECT
  round(sum(line_item_unblended_cost) FILTER (WHERE coalesce(tags['iamPrincipal/owner'], '') <> ''), 2),
  round(sum(line_item_unblended_cost) FILTER (WHERE coalesce(tags['iamPrincipal/owner'], '') = ''), 2)
FROM read_parquet('{files}')
WHERE coalesce(line_item_iam_principal, '') <> ''
"""


def main():
    if len(sys.argv) not in (2, 3) or not sys.argv[1].startswith("s3://"):
        sys.exit(__doc__)
    prefix = sys.argv[1].rstrip("/")
    month = sys.argv[2] if len(sys.argv) == 3 else datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m")
    src = f"{prefix}/data/BILLING_PERIOD={month}/"

    with tempfile.TemporaryDirectory() as tmp:
        subprocess.run(["aws", "s3", "cp", "--recursive", "--quiet", src, tmp,
                        "--exclude", "*", "--include", "*.parquet"], check=True)
        files = f"{tmp}/*.parquet"
        try:
            tracked, untracked = duckdb.sql(TOTALS.format(files=files)).fetchone()
        except duckdb.IOException:
            sys.exit(f"error: no export files found under {src}")
        print(f"Bedrock model spend in {month}: tracked ${tracked or 0}, untracked ${untracked or 0}\n")
        rows = duckdb.sql(QUERY.format(files=files)).fetchall()
        if not rows:
            print("Everyone used their personal role. Nothing to follow up.")
            return
        print(f"{'WHO':<30} {'USED VIA':<52} {'USD':>9}  LAST USED")
        for who, via, usd, last in rows:
            print(f"{who:<30} {via:<52} {usd:>9.2f}  {last}")


if __name__ == "__main__":
    main()
