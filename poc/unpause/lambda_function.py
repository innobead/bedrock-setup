import time

import boto3

budgets = boto3.client("budgets")
ACCOUNT = boto3.client("sts").get_caller_identity()["Account"]


def _status(name, action_id):
    return budgets.describe_budget_action(
        AccountId=ACCOUNT, BudgetName=name, ActionId=action_id)["Action"]["Status"]


def _run(name, action_id, execution_type):
    budgets.execute_budget_action(
        AccountId=ACCOUNT, BudgetName=name, ActionId=action_id, ExecutionType=execution_type)


def handler(event, context):
    """Unpause everyone on the 1st of the month and re-arm their budget actions.

    REVERSE detaches the deny policy but leaves the action in REVERSE_SUCCESS, where it does not
    fire again. RESET puts it back to STANDBY so it can pause the person again this month.
    """
    unpaused, rearmed = [], []
    for page in budgets.get_paginator("describe_budget_actions_for_account").paginate(AccountId=ACCOUNT):
        for a in page["Actions"]:
            name, action_id, status = a["BudgetName"], a["ActionId"], a["Status"]
            if not name.startswith("bedrock-"):
                continue
            if status == "EXECUTION_SUCCESS":
                _run(name, action_id, "REVERSE_BUDGET_ACTION")
                for _ in range(30):
                    status = _status(name, action_id)
                    if status != "REVERSE_IN_PROGRESS":
                        break
                    time.sleep(1)
                unpaused.append(name)
            if status == "REVERSE_SUCCESS":
                _run(name, action_id, "RESET_BUDGET_ACTION")
                rearmed.append(name)
    print("unpaused:", unpaused, "rearmed:", rearmed)
    return {"unpaused": unpaused, "rearmed": rearmed}
