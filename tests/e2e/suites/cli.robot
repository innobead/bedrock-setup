*** Settings ***
Documentation       Smoke test of the two built Linux binaries, with no AWS config or credentials at all.
...                 Config validation, flags and output formats are covered by the Go unit tests; this
...                 suite checks only that the shipped binaries run and that the main flow works end to end
...                 up to the first AWS call.
Resource            ../resources/common.resource
Suite Setup         Start Run    needs_aws=False
Test Tags           cli


*** Test Cases ***
Both Binaries Run
    ${out}=    Admin    --version    config=none
    Should Start With    ${out}    bedrock-admin version
    ${out}=    Bedrock    --version
    Should Start With    ${out}    bedrock version
    ${out}=    Admin    --help    config=none
    Should Contain    ${out}    bedrock-admin plan
    ${out}=    Bedrock    --help
    Should Contain    ${out}    bedrock setup

Configure Then Plan Stops At The AWS Call
    [Documentation]    configure writes a file that loads; plan then fails on the missing credentials (exit 1).
    ${f}=    Set Variable    ${OUTPUT DIR}/work/configured.yaml
    Admin    configure    --yes    --account    111122223333    --region    eu-central-1    --product    p
    ...    --id    e2e-cli    config=${f}
    File Should Exist    ${f}
    ${r}=    Admin    plan    config=${f}    rc=1    output=result
    Should Not Contain    ${r.stderr}    invalid config
    Should Contain    ${r.stderr}    AWS credentials
    ${d}=    Admin    doctor    --json    config=${f}    rc=2
    Should Be True    $d["checks"][0]["ok"]
    Should Be Equal    ${d}[checks][1][name]    credentials

User CLI Without A Profile
    ${d}=    Bedrock    doctor    --json    rc=2
    Should Be Equal    ${d}[checks][0][fix]    bedrock setup
    ${r}=    Bedrock    setup    --region    us-west-2    rc=1    output=result
    Should Contain    ${r.stderr}    no SSO profile
