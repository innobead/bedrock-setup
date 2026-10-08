*** Settings ***
Documentation       What a user does: bedrock setup, calls through the personal role, bedrock doctor, the
...                 Step 1.7 block on direct SSO calls, and a manual pause seen from the user's side.
...                 Needs SSO_PROFILE signed in (aws sso login) in the same account. The runner is added as
...                 a user named <run>-me; the user CLI writes to a copy of ~/.aws/config, never the real one.
Resource            ../resources/common.resource
Suite Setup         Start User Access Run
Suite Teardown      Remove Run Users
Test Tags           user-access


*** Variables ***
${PROFILE}      bedrock


*** Test Cases ***
Setup Writes A Working Profile
    ${r}=    Bedrock    setup    --sso-profile    ${SSO_PROFILE}    --role-arn    ${ROLE_ARN}    --region    ${REGION}
    ...    --profile-name    ${PROFILE}    output=result    rc=any
    Should Be Equal As Integers    ${r.rc}    0    setup or its doctor failed:\n${r.stdout}\n${r.stderr}
    ${text}=    User Aws Config Text
    Should Contain    ${text}    [profile ${PROFILE}]
    Should Contain    ${text}    role_session_name = ${SSO_EMAIL}
    Should Contain    ${text}    source_profile = ${SSO_PROFILE}

Re-running Setup Asks Before Replacing And Keeps A Backup
    ${r}=    Bedrock    setup    --sso-profile    ${SSO_PROFILE}    --role-arn    ${ROLE_ARN}    --region    us-east-1
    ...    --profile-name    ${PROFILE}    rc=1    output=result
    Should Contain    ${r.stderr}    --yes
    ${text}=    User Aws Config Text
    Should Not Contain    ${text}    region = us-east-1
    Bedrock    setup    --sso-profile    ${SSO_PROFILE}    --role-arn    ${ROLE_ARN}    --region    us-east-1
    ...    --profile-name    ${PROFILE}    --yes    rc=any
    File Should Exist    ${USER_AWS_CONFIG}.bak
    ${backup}=    Get File    ${USER_AWS_CONFIG}.bak
    Should Contain    ${backup}    region = ${REGION}
    Bedrock    setup    --sso-profile    ${SSO_PROFILE}    --role-arn    ${ROLE_ARN}    --region    ${REGION}
    ...    --profile-name    ${PROFILE}    --yes

The Personal Role Works
    ${arn}=    Identity    ${PROFILE}
    ${role}=    Role Of    me
    Should Contain    ${arn}    assumed-role/${role}/${SSO_EMAIL}
    FOR    ${m}    IN    @{MODELS}
        Model Call Should Work    ${PROFILE}    ${m}
    END

Doctor Passes
    ${d}=    Bedrock    doctor    --profile-name    ${PROFILE}    --json
    Should Be True    $d["ok"]

Direct SSO Calls Are Denied
    [Documentation]    Step 1.7. Skipped with a note when the org admin hasn't applied the deny to the
    ...    runner's permission set in the test account.
    ${denied}=    Model Call Is Denied    sso
    Skip If    not ${denied}    Step 1.7 isn't applied to permission set ${SSO_PERMISSION_SET}: direct SSO calls work
    Model Call Should Be Denied    sso    explicit=True

A Manual Pause Denies Calls And Unpause Restores Them
    Admin    pause    ${SSO_EMAIL}    --reason    e2e user-access
    Wait Until Keyword Succeeds    3 min    10 s    Model Call Should Be Denied    ${PROFILE}    explicit=True
    ${d}=    Bedrock    doctor    --profile-name    ${PROFILE}    --json    rc=2
    ${fixes}=    Evaluate    " ".join(c.get("fix", "") for c in $d["checks"] if not c["ok"])
    Should Contain    ${fixes}    paused
    Admin    unpause    ${SSO_EMAIL}
    Wait Until Keyword Succeeds    3 min    10 s    Model Call Should Work    ${PROFILE}
    [Teardown]    Admin    unpause    ${SSO_EMAIL}    rc=any


*** Keywords ***
Start User Access Run
    Start AWS Run
    Start SSO
    Add User    me
    Apply
    ${role}=    Role Of    me
    Set Suite Variable    ${ROLE_ARN}    arn:aws:iam::${ACCOUNT}:role/bedrock-users/${role}
    Wait Until Sso Can Assume    ${ROLE_ARN}
    ${ps}=    Get Sso Permission Set
    Set Suite Variable    ${SSO_PERMISSION_SET}    ${ps}
