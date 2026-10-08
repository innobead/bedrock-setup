*** Settings ***
Documentation       Personal roles, budgets and pause actions: onboarding, changes in place, offboarding,
...                 manual pause and unpause. Uses its own id (e2e-<run>); needs the account set up first.
Resource            ../resources/common.resource
Suite Setup         Start AWS Run
Suite Teardown      Remove Run Users
Test Tags           users


*** Test Cases ***
Add A User
    Add User    a
    ${r}=    Apply
    User Should Be Set Up    a    1    100    80
    ${email}=    User Email    a
    ${cmd}=    Evaluate    [c["command"] for c in $r.json["setup_commands"] if c["email"] == $email]
    Should Start With    ${cmd}[0]    bedrock setup
    Plan Should Show No Changes

Defaults And Overrides
    Set Config    defaults.pause_at_percent    90
    Set Config    defaults.alert_at_percent    [50, 75]
    Add User    b    limit_usd=3    pause_at_percent=120    alert_at_percent=[60]
    Add User    c
    Apply
    User Should Be Set Up    a    1    90    50    75
    User Should Be Set Up    b    3    120    60
    User Should Be Set Up    c    1    90    50    75
    Plan Should Show No Changes

No Alerts
    Set User    c    alert_at_percent    []
    Apply
    User Should Be Set Up    c    1    90

Changes Keep The Same Budget And Action
    ${budget}=    Budget Of    b
    ${before}=    Budget Action Id    ${budget}
    Set User    b    limit_usd    5
    Set User    b    pause_at_percent    150
    Set User    b    alert_at_percent    [70, 90]
    ${i}=    User Plan Op Should Be    b    update
    ${details}=    Evaluate    ", ".join($i["details"])
    Should Contain    ${details}    limit 3 → 5
    Should Contain    ${details}    pause_at 120% → 150%
    Should Not Be True    ${i.get("needs_yes", False)}
    Apply
    User Should Be Set Up    b    5    150    70    90
    ${after}=    Budget Action Id    ${budget}
    Should Be Equal    ${after}    ${before}    the pause action was recreated

Plan Flags A Change That Recreates The Action
    Change Budget Action Directly    b
    ${i}=    User Plan Op Should Be    b    update
    Should Contain    ${i}[details]    pause action changed outside the file, recreate it
    Should Be Equal    ${i}[needs_yes]    ${True}
    ${r}=    Admin    apply    rc=1    output=result
    Should Contain    ${r.stderr}    need --yes
    Apply
    User Should Be Set Up    b    5    150    70    90

Drift In A Budget Is Fixed
    ${budget}=    Budget Of    a
    Set Budget Limit Directly    ${budget}    7
    ${i}=    User Plan Op Should Be    a    update
    Should Contain    ${i}[details]    limit 7 → 1
    Apply
    Budget Limit Should Be    ${budget}    1

Removing A User Needs Yes
    Remove User    c
    ${i}=    User Plan Op Should Be    c    delete
    Should Be Equal    ${i}[needs_yes]    ${True}
    ${r}=    Admin    apply    rc=1    output=result
    Should Contain    ${r.stderr}    need --yes
    ${role}=    Role Of    c
    Role Should Exist    ${role}
    Admin    apply    --no-delete
    Role Should Exist    ${role}
    ${p}=    Plan    --no-delete
    ${email}=    User Email    c
    ${i}=    Plan Item    ${p}    ${email}
    Should Be Equal    ${i}[status]    offboard skipped (--no-delete)
    Apply
    User Should Be Gone    c

Pause By Hand
    ${role}=    Role Of    a
    ${email}=    User Email    a
    ${r}=    Admin    pause    ${email}    --reason    e2e test    --json
    Should Be Equal    ${r}[result]    paused
    Deny Policy Should Be Attached    ${role}
    Role Tag Should Be    ${role}    bedrock:pause-reason    e2e test
    Pause State Should Be    a    paused-by-hand

Apply Leaves A Manual Pause
    Apply
    ${role}=    Role Of    a
    Deny Policy Should Be Attached    ${role}
    Pause State Should Be    a    paused-by-hand

The Monthly Lambda Leaves A Manual Pause And Saves A Snapshot
    ${result}=    Invoke Monthly Lambda
    Pause State Should Be    a    paused-by-hand
    ${role}=    Role Of    a
    Deny Policy Should Be Attached    ${role}
    Pause State Should Be    b    armed
    Lambda Snapshot Should Exist    ${result}
    ${email}=    User Email    a
    ${u}=    Evaluate    [i for i in $result["snapshot"]["users"] if i["email"] == $email][0]
    Should Be Equal    ${u}[state]    paused-by-hand
    Should Be Equal As Numbers    ${u}[limit_usd]    1
    [Teardown]    Restore Lambda Snapshot

Unpause
    ${role}=    Role Of    a
    ${email}=    User Email    a
    ${r}=    Admin    unpause    ${email}    --reason    done    --json
    Should Be Equal    ${r}[result]    removed-manual-pause
    Deny Policy Should Not Be Attached    ${role}
    Role Should Not Have Tag    ${role}    bedrock:paused-by
    Role Tag Should Be    ${role}    bedrock:unpause-reason    done
    Pause State Should Be    a    armed

Unpause An Armed User Changes Nothing
    ${email}=    User Email    b
    ${r}=    Admin    unpause    ${email}    --json
    Should Be Equal    ${r}[result]    not-paused
    Pause State Should Be    b    armed

Offboard Everyone
    Set Users
    Apply
    User Should Be Gone    a
    User Should Be Gone    b
