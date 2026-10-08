*** Settings ***
Documentation       Shared keywords. The library (lib/BedrockE2E.py) runs the CLIs and reads AWS directly.
Library             Collections
Library             OperatingSystem
Library             String
Library             ../lib/BedrockE2E.py


*** Keywords ***
Start AWS Run
    [Documentation]    Starts a run that uses the admin profile. The account setup must exist (run the
    ...    account suite once); this run only adds and removes its own users.
    Start Run
    ${owner}=    Account Resource Owner
    IF    not $owner
        Fail    The account isn't set up: run ./run.sh account first
    END
    Log    Account setup managed by id ${owner}    console=True

Remove Run Users
    [Documentation]    Cleanup: apply the run's config with users: [].
    Set Users
    Run Keyword And Ignore Error    Admin    apply    --yes

Apply
    [Documentation]    apply --yes --json; returns the result (rc, stdout, stderr, json).
    [Arguments]    @{args}    ${rc}=0    ${config}=${EMPTY}
    ${r}=    Admin    apply    --yes    --json    @{args}    rc=${rc}    config=${config}    output=result
    RETURN    ${r}

Plan
    [Arguments]    @{args}    ${rc}=any
    ${p}=    Admin    plan    --json    @{args}    rc=${rc}
    RETURN    ${p}

User Plan Op Should Be
    [Arguments]    ${x}    ${op}
    ${p}=    Plan
    ${email}=    User Email    ${x}
    ${item}=    Plan Op Should Be    ${p}    ${email}    ${op}
    RETURN    ${item}

Plan Should Show No Changes
    ${p}=    Plan    rc=0
    Plan Should Have No Changes    ${p}

User Should Be Set Up
    [Documentation]    Role, budget and pause action exist, with the given threshold and alerts.
    [Arguments]    ${x}    ${limit}    ${pause_at}=100    @{alerts}
    ${role}=    Role Of    ${x}
    ${budget}=    Budget Of    ${x}
    ${email}=    User Email    ${x}
    Role Should Exist    ${role}
    Role Tag Should Be    ${role}    owner    ${email}
    Role Tag Should Be    ${role}    bedrock-admin:id    ${RUN_CONFIG_ID}
    Role Should Have Trust For    ${role}    ${email}
    Budget Limit Should Be    ${budget}    ${limit}
    Budget Alerts Should Be    ${budget}    @{alerts}
    Budget Action Should Be    ${budget}    threshold=${pause_at}    status=STANDBY
    Deny Policy Should Not Be Attached    ${role}

User Should Be Gone
    [Arguments]    ${x}
    ${role}=    Role Of    ${x}
    ${budget}=    Budget Of    ${x}
    Role Should Not Exist    ${role}
    Budget Should Not Exist    ${budget}

Text Should Contain Lines
    [Arguments]    ${text}    @{lines}
    FOR    ${l}    IN    @{lines}
        Should Contain    ${text}    ${l}
    END
