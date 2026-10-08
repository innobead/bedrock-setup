*** Settings ***
Documentation       Everything that waits for AWS billing, in phases that hand over through
...                 results/billing/<run>.json (see tests/e2e/README.md):
...                 phase1 any time; phase2 12+ h later; phase3 24+ h after phase 1; manual after phase 2.
...                 Too early, or billing not caught up yet: skipped with "retry after <time>"; 48 h after
...                 phase 1 with no result: failed. A full run costs about $4 in model calls.
...
...                 Test users spend through their own personal role: a test-only trust statement lets the
...                 admin profile assume it with session name <email> (only the runner's own SSO login
...                 could otherwise), so the role's owner tag attributes the spend as for a real user.
Resource            ../resources/common.resource
Test Tags           billing


*** Test Cases ***
Phase 1: Spend
    [Tags]    phase1
    Start AWS Run
    FOR    ${x}    IN    a    b    c    d
        Add User    ${x}    limit_usd=1
    END
    IF    $NOTIFY_EMAIL    Set User    a    notify    ${NOTIFY_EMAIL}
    Apply
    Start Billing Phase1
    FOR    ${x}    IN    a    b    c    d
        Let Admin Assume    ${x}
    END
    ${a}=    Spend    user:a    1.20
    ${b}=    Spend    user:b    1.20
    ${c}=    Spend    user:c    0.10
    ${d}=    Spend    user:d    1.20
    Model Call Should Work    untracked
    ${untracked}=    Identity    untracked
    Save Billing State    spent={"a": ${a}, "b": ${b}, "c": ${c}, "d": ${d}}    untracked_caller=${untracked}
    Mark Phase Done    phase1

Phase 2: Paused
    [Tags]    phase2
    Load Run    12
    ${states}=    Pause States    a    b    d
    ${ok}=    Evaluate    all(v == "paused" for v in $states.values())
    Billing Caught Up Or Skip    ${ok}    a, b and d paused (${states})
    FOR    ${x}    IN    a    b    d
        ${role}=    Role Of    ${x}
        Deny Policy Should Be Attached    ${role}
        ${budget}=    Budget Of    ${x}
        Budget Action Should Be    ${budget}    status=EXECUTION_SUCCESS
    END
    Model Call Should Be Denied    user:a    explicit=True
    Pause State Should Be    c    armed
    Model Call Should Work    user:c

    # d: unpaused by hand, so off for the rest of the month; more spend doesn't pause it.
    ${d_email}=    User Email    d
    ${r}=    Admin    unpause    ${d_email}    --json
    Should Be Equal    ${r}[result]    off-until-1st
    Pause State Should Be    d    off
    Wait Until Keyword Succeeds    3 min    10 s    Model Call Should Work    user:d
    ${more}=    Spend    user:d    0.20
    Save Billing State    spent_d_more=${more}

    # b: a higher limit that still leaves the trigger at or below spend keeps b paused.
    Set User    b    limit_usd    1.10
    ${i}=    User Plan Op Should Be    b    update
    ${notes}=    Evaluate    " ".join($i["notes"] or [])
    Should Contain    ${notes}    stays paused
    Apply
    Pause State Should Be    b    paused
    # pause_at_percent 200 moves the trigger to $2.20: apply unpauses b and re-arms it at 200 %.
    Set User    b    pause_at_percent    200
    Apply
    Pause State Should Be    b    armed
    ${budget}=    Budget Of    b
    Budget Action Should Be    ${budget}    threshold=200    status=STANDBY
    Wait Until Keyword Succeeds    3 min    10 s    Model Call Should Work    user:b

    # The monthly Lambda re-arms everyone who is paused or off and saves the snapshot.
    ${result}=    Invoke Monthly Lambda
    Lambda Snapshot Should Exist    ${result}
    Pause State Should Be    a    armed
    Pause State Should Be    d    armed
    ${role}=    Role Of    a
    Deny Policy Should Not Be Attached    ${role}
    Save Billing State    lambda_at=${{ datetime.datetime.now(datetime.timezone.utc).isoformat() }}
    Mark Phase Done    phase2

Phase 3: Reported
    [Tags]    phase3
    Load Run    24
    ${st}=    Billing State
    Skip If    "phase2" not in $st["phases"]    run phase 2 first
    # Re-armed by the Lambda with spend already over the trigger: paused again at the next update.
    ${states}=    Pause States    a    d
    ${again}=    Evaluate    all(v == "paused" for v in $states.values())
    Billing Caught Up Or Skip    ${again}    a and d paused again after the Lambda re-armed them
    # Spend in the cost export matches what phase 1 spent, within billing rounding.
    ${u}=    Admin    usage    --json    rc=any
    ${seen}=    Evaluate    all(r["spent_usd"] > 0 for r in $u["users"])
    Billing Caught Up Or Skip    ${seen}    the cost export has spend for every test user
    FOR    ${x}    IN    a    b    c
        ${row}=    User Row    ${u}    ${x}
        ${want}=    Set Variable    ${st}[spent][${x}]
        Should Be Close    ${row}[spent_usd]    ${want}    ${{ max(0.15, 0.25 * float($want)) }}
    END
    ${row}=    User Row    ${u}    d
    ${want}=    Evaluate    $st["spent"]["d"] + $st.get("spent_d_more", 0)
    Should Be Close    ${row}[spent_usd]    ${want}    ${{ max(0.15, 0.25 * float($want)) }}
    ${unt}=    Admin    usage    --untracked    --json    rc=2
    ${who}=    Evaluate    " ".join(c["who"] for c in $unt["untracked"])
    ${caller}=    Evaluate    $st["untracked_caller"].rstrip("/").split("/")[-1]
    Should Contain    ${who}    ${caller}
    Remove Run Users
    Delete Billing State

Manual: Budget Email Arrived
    [Tags]    manual
    Load Run    12
    Skip If    not $NOTIFY_EMAIL    NOTIFY_EMAIL is not set
    ${a_email}=    User Email    a
    Confirm    Did ${NOTIFY_EMAIL} receive an AWS Budgets email for ${a_email}?


*** Keywords ***
Load Run
    [Arguments]    ${hours}
    Start AWS Run
    Load Billing State
    Require Hours Since Phase1    ${hours}
