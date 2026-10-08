*** Settings ***
Documentation       The account setup (Steps 1.1-1.7): apply, plan, drift, the Step 1.7 handoff, doctor,
...                 export, id safety and uninstall. This suite owns the account setup with id ACCOUNT_ID
...                 and leaves it in place at the end, for the other suites.
Resource            ../resources/common.resource
Suite Setup         Start Account Run
Suite Teardown      Leave The Account Set Up
Test Tags           account


*** Test Cases ***
Apply Creates The Account Setup
    ${r}=    Apply    rc=any
    Should Be True    ${r.rc} in (0, 2)    apply failed: ${r.stderr}
    Bucket Should Exist
    Cost Export Should Exist
    Deny Policy Should Exist
    ${owner}=    Account Resource Owner
    Should Be Equal    ${owner}    ${ACCOUNT_ID}
    Role Should Exist    bedrock-budget-actions
    Role Should Exist    bedrock-monthly-unpause
    Role Should Exist    bedrock-monthly-unpause-scheduler
    Lambda Should Exist
    Schedule Should Exist
    ${p}=    Plan
    FOR    ${m}    IN    @{MODELS}
        Plan Op Should Be    ${p}    model ${m}    ok
    END
    ${tag}=    Plan Item    ${p}    cost allocation tag iamPrincipal/owner
    IF    $tag["op"] == "needs-org-admin"
        Should Contain    ${tag}[status]    management account
    ELSE
        Cost Allocation Tag Should Be Active
    END

Second Apply Changes Nothing
    ${p}=    Plan
    Plan Should Have No Changes But The Handoff    ${p}
    ${a}=    Apply    rc=any
    ${ops}=    Evaluate    sorted({x["op"] for x in $a.json["applied"]})
    Should Be True    $ops in ([], ["needs-org-admin"])    second apply changed: ${a.json}[applied]

Plan Names Exactly The Missing Piece
    Delete Monthly Schedule
    ${p}=    Plan    rc=2
    ${pending}=    Changes But Handoffs    ${p}
    Should Be Equal    ${pending}    ${{ ["schedule bedrock-monthly-unpause"] }}
    Plan Op Should Be    ${p}    schedule bedrock-monthly-unpause    create
    ${out}=    Admin    plan    rc=2
    Should Contain    ${out}    + 1.4 schedule bedrock-monthly-unpause
    Apply    rc=any
    Schedule Should Exist

Doctor Names The Missing Piece
    Delete Monthly Schedule
    ${d}=    Admin    doctor    --json    rc=2
    ${bad}=    Evaluate    [c["name"] for c in $d["checks"] if not c["ok"]]
    Should Contain    ${bad}    Step 1.4 schedule bedrock-monthly-unpause
    Apply    rc=any
    Schedule Should Exist

Doctor Passes
    ${d}=    Admin    doctor    --json    rc=any
    ${bad}=    Evaluate    [c["name"] for c in $d["checks"] if not c["ok"] and "org admin" not in (c.get("detail", "") + c.get("fix", "")) and c["name"] != "cost export data"]
    Should Be Empty    ${bad}    doctor failed: ${d}
    ${names}=    Evaluate    [c["name"] for c in $d["checks"]]
    Should Contain    ${names}    credentials
    Should Contain    ${names}    permissions

Drift Is Shown By Plan And Fixed By Apply
    Change Lambda Setting Directly
    ${p}=    Plan    rc=2
    ${i}=    Plan Op Should Be    ${p}    Lambda bedrock-monthly-unpause    update
    Should Contain    ${i}[details]    settings changed outside the file
    Apply    rc=any
    ${prefix}=    Lambda Setting    SNAPSHOT_PREFIX
    Should Not Contain    ${prefix}    e2e-drift
    ${p}=    Plan
    Plan Op Should Be    ${p}    Lambda bedrock-monthly-unpause    ok

Step 1.7 Handoff
    [Documentation]    apply writes the policy file. plan says "needs org admin" until every permission set
    ...    but the admin one carries the deny (an org admin sets that up once in the test account).
    ${p}=    Plan
    ${i}=    Plan Item    ${p}    block direct model calls
    IF    $i["op"] == "needs-org-admin"
        Block Policy File Should Exist
        Should Contain    ${i}[status]    needs org admin
        IF    $BLOCK_TEST_PERMISSION_SET
            Should Not Contain    ${i}[status]    ${BLOCK_TEST_PERMISSION_SET}
            ...    msg=${BLOCK_TEST_PERMISSION_SET} doesn't carry the deny: ${i}[status]
        END
        Skip    Step 1.7 not done by the org admin for every permission set: ${i}[status]
    END
    Should Be Equal    ${i}[op]    ok
    Should Start With    ${i}[status]    ok

Export Then Plan Shows No Changes
    ${models}=    Evaluate    [a for m in $MODELS for a in ("--model", m)]
    ${yaml}=    Admin    export    --id    ${ACCOUNT_ID}    --region    ${REGION}    --product    ${PRODUCT}    @{models}
    ...    config=none
    ${f}=    Write Raw Config    ${yaml}    exported.yaml
    ${p}=    Admin    plan    --json    config=${f}    rc=any
    Plan Should Have No Changes But The Handoff    ${p}

Roles With Another Id Are Left Alone
    [Documentation]    A role and budget made with another id are "not managed" here, and apply --yes keeps them.
    ${other}=    Set Variable    ${RUN}-other
    ${f}=    Write Config With Id    ${other}    x
    Admin    apply    --yes    config=${f}    rc=any
    ${role}=    Role Of    x
    Role Should Exist    ${role}
    ${budget}=    Budget Of    x
    Role Tag Should Be    ${role}    bedrock-admin:id    ${other}
    ${p}=    Plan
    ${email}=    User Email    x
    ${i}=    Plan Op Should Be    ${p}    ${email}    info
    Should Be Equal    ${i}[status]    not managed (id ${other})
    Apply    rc=any
    Role Should Exist    ${role}
    Add User    x
    ${p}=    Plan    rc=2
    ${i}=    Plan Op Should Be    ${p}    ${email}    conflict
    Should Contain    ${i}[details][0]    is managed by ${other}; left alone
    Apply    rc=2
    Role Tag Should Be    ${role}    bedrock-admin:id    ${other}
    Budget Limit Should Be    ${budget}    1
    [Teardown]    Run Keywords    Remove User    x
    ...    AND    Write Config With Id    ${other}    name=${other}.yaml
    ...    AND    Admin    apply    --yes    config=${OUTPUT DIR}/work/${other}.yaml    rc=any
    ...    AND    User Should Be Gone    x

Roles Without An Id Are Left Alone
    ${role}=    Create Unmanaged Role    y
    ${p}=    Plan
    ${email}=    User Email    y
    ${i}=    Plan Op Should Be    ${p}    ${email}    info
    Should Be Equal    ${i}[status]    not managed (role ${role} has no bedrock-admin:id tag)
    Apply    rc=any
    Role Should Exist    ${role}
    [Teardown]    Delete Role    ${role}

Uninstall Refuses While Users Remain
    Add User    a
    Apply    rc=any
    ${r}=    Admin    uninstall    --yes    rc=1    output=result
    Should Contain    ${r.stderr}    personal roles remain
    Lambda Should Exist
    [Teardown]    Run Keywords    Set Users    AND    Apply    rc=any

Uninstall Keeps The Bucket
    Skip If Real Users
    Admin    uninstall    --yes
    Lambda Should Not Exist
    Deny Policy Should Not Exist
    Role Should Not Exist    bedrock-budget-actions
    Bucket Should Exist
    [Teardown]    Apply    rc=any

Uninstall With Delete Data Removes The Bucket
    Skip If Real Users
    Skip If    not ${ALLOW_DELETE_DATA}    ALLOW_DELETE_DATA is False: the bucket holds the billing history
    Admin    uninstall    --yes    --delete-data
    Lambda Should Not Exist
    ${exists}=    Bucket Exists
    Should Not Be True    ${exists}
    [Teardown]    Apply    rc=any


*** Keywords ***
Start Account Run
    Start Run
    Use Account Id
    ${owner}=    Account Resource Owner
    IF    $owner and $owner != $ACCOUNT_ID
        Fail    The account setup is managed by id ${owner}: set ACCOUNT_ID to it, or use a test account
    END

Leave The Account Set Up
    Set Users
    Apply    rc=any

Plan Should Have No Changes But The Handoff
    [Arguments]    ${p}
    ${pending}=    Changes But Handoffs    ${p}
    Should Be Empty    ${pending}    plan has changes: ${pending}

Bucket Should Exist
    ${exists}=    Bucket Exists
    Should Be True    ${exists}    no bucket

Skip If Real Users
    ${others}=    Non Test Personal Roles
    Skip If    ${others}    the account has real users (${others}): uninstall would refuse
