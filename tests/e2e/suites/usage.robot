*** Settings ***
Documentation       bedrock-admin usage against a real bucket: the Parquet fixtures in tests/fixtures are
...                 uploaded as this and last month's cost export under e2e/<run>/, with last month's
...                 snapshot. The users usage-a/b/c@example.test are applied for real, so this month's
...                 limits and pause state come from their budgets. Numbers: tests/fixtures/README.md.
...                 Report layout and formatting are covered by the Go unit tests.
Resource            ../resources/common.resource
Suite Setup         Start Usage Run
Suite Teardown      Finish Usage Run
Test Tags           usage


*** Variables ***
${A}        usage-a@example.test
${B}        usage-b@example.test
${C}        usage-c@example.test


*** Test Cases ***
This Month Splits Tracked And Untracked
    ${u}=    Admin    usage    --json    config=${USAGE_CONFIG}    rc=2
    Should Be Equal    ${u}[month]    ${THIS_MONTH}
    Should Be Close    ${u}[tracked_usd]    18.45
    Should Be Close    ${u}[untracked_usd]    7.60
    Length Should Be    ${u}[untracked]    6
    ${a}=    User Row    ${u}    ${A}
    Should Be Close    ${a}[spent_usd]    13.25
    Should Be Close    ${a}[limit_usd]    10
    Should Be Equal    ${a}[pause]    armed
    ${b}=    User Row    ${u}    ${B}
    Should Be Close    ${b}[spent_usd]    4.00

Only Tracked Exits 0
    ${u}=    Admin    usage    --tracked    --json    config=${USAGE_CONFIG}
    Should Be Close    ${u}[tracked_usd]    18.45

One Person This Month
    ${d}=    Admin    usage    ${A}    --json    config=${USAGE_CONFIG}
    Should Be Close    ${d}[total_usd]    13.85
    Length Should Be    ${d}[callers]    2
    ${d}=    Admin    usage    usage-sso@example.test    --json    config=${USAGE_CONFIG}
    Should Be Close    ${d}[total_usd]    2.70
    Dictionary Should Not Contain Key    ${d}    user

Last Month Uses The Snapshot
    ${u}=    Admin    usage    --month    ${LAST_MONTH}    --json    config=${USAGE_CONFIG}    rc=2
    Should Be Close    ${u}[tracked_usd]    75.00
    Should Be Close    ${u}[untracked_usd]    1.00
    ${a}=    User Row    ${u}    ${A}
    Should Be Close    ${a}[limit_usd]    50
    Should Be Equal    ${a}[pause]    paused
    ${c}=    User Row    ${u}    ${C}
    Should Be Equal    ${c}[pause]    paused-by-hand
    Should Be Close    ${c}[spent_usd]    0

Several Months
    ${m}=    Admin    usage    --months    2    --json    config=${USAGE_CONFIG}    rc=2
    Should Be Close    ${m}[tracked_totals][${THIS_MONTH}]    18.45
    Should Be Close    ${m}[tracked_totals][${LAST_MONTH}]    75.00
    ${a}=    Evaluate    [s for s in $m["users"] if s["who"].lower() == $A][0]
    Should Be Close    ${a}[total_usd]    68.25


*** Keywords ***
Start Usage Run
    Start AWS Run
    Add User    ${A}    limit_usd=10
    Add User    ${B}    limit_usd=10
    Add User    ${C}    limit_usd=10
    Apply
    ${f}=    Upload Usage Fixtures
    Set Suite Variable    ${USAGE_CONFIG}    ${f}

Finish Usage Run
    Run Keyword And Ignore Error    Delete Prefix    e2e/${RUN}/
    Remove Run Users
