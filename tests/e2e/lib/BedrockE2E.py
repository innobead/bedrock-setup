"""Robot Framework library for the bedrock-admin e2e tests.

The CLIs under test are run as subprocesses. Every check reads AWS directly with boto3 and never
trusts the CLI's own output. Settings come from the variable file (see config/example.py).

Each run has an id, e2e-<6 hex>, used as the config's id: and in the test users' names
(e2e-<hex>-<x>), so a run only ever changes its own users.
"""
import datetime as dt
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import time
from pathlib import Path

import boto3
from botocore.exceptions import ClientError
from robot.api import Failure, SkipExecution, logger
from robot.libraries.BuiltIn import BuiltIn

HERE = Path(__file__).resolve().parent.parent
BIN = Path(os.environ.get("E2E_BIN", HERE / "bin"))
FIXTURES = Path(os.environ.get("E2E_FIXTURES", HERE.parent / "fixtures"))
STATE_DIR = HERE / "results" / "billing"

LAMBDA = "bedrock-monthly-unpause"
SCHEDULE = "bedrock-monthly-unpause"
DENY_POLICY = "bedrock-deny"
ID_TAG = "bedrock-admin:id"
OWNER_TAG = "owner"
PAUSED_BY_TAG = "bedrock:paused-by"
PAUSE_REASON_TAG = "bedrock:pause-reason"
PENDING_OPS = {"create", "update", "delete", "needs-org-admin", "conflict"}

DEFAULTS = {
    "ADMIN_PROFILE": "default",
    "REGION": "us-west-2",
    "MODELS": [],
    "ACCOUNT_ID": "e2e",
    "PRODUCT": "bedrock-e2e",
    "CUR_BUCKET": "",
    "ADMIN_PERMISSION_SET": "BedrockAdmin",
    "BLOCK_TEST_PERMISSION_SET": "",
    "ALLOW_DELETE_DATA": False,
    "SSO_PROFILE": "",
    "UNTRACKED_PROFILE": "",
    "TEST_EMAIL_DOMAIN": "example.com",
    "NOTIFY_EMAIL": "",
    "RUN_ID": "",
    "INTERACTIVE": True,
}

# USD per million input and output tokens, to estimate what a call cost.
PRICES = {"opus-4-5": (5, 25), "opus": (15, 75), "sonnet": (3, 15), "haiku": (1, 5)}


def _bool(v):
    return v if isinstance(v, bool) else str(v).strip().lower() in ("1", "true", "yes", "y")


def _value(v):
    """Turns a Robot argument into a config value: numbers, booleans, lists and null as JSON."""
    if not isinstance(v, str):
        return v
    try:
        return json.loads(v)
    except ValueError:
        return v


def _console(msg):
    sys.__stdout__.write(msg + "\n")
    sys.__stdout__.flush()


def default_models(region):
    geo = region.split("-", 1)[0]
    geo = geo if geo in ("us", "eu") else "global"
    return [f"{geo}.anthropic.claude-{m}-5-5" for m in ("opus", "sonnet", "haiku")]


class Result:
    def __init__(self, rc, out, err):
        self.rc, self.stdout, self.stderr = rc, out, err

    @property
    def json(self):
        try:
            return json.loads(self.stdout)
        except ValueError as e:
            raise Failure(f"not JSON: {e}\n{self.stdout}")


class BedrockE2E:
    ROBOT_LIBRARY_SCOPE = "GLOBAL"

    def __init__(self):
        self.s = None
        self.cfg = None
        self.run_id = ""
        self.account = ""
        self.work = None
        self.sso_email = ""
        self.sso_permission_set = ""
        self.user_aws_config = None
        self.offline = False
        self._sessions = {}
        self._user_sessions = {}

    # ---------------------------------------------------------------- settings and run

    def _settings(self):
        if self.s is None:
            bi = BuiltIn()
            self.s = {k: bi.get_variable_value("${%s}" % k, d) for k, d in DEFAULTS.items()}
            self.s["INTERACTIVE"] = _bool(self.s["INTERACTIVE"])
            self.s["ALLOW_DELETE_DATA"] = _bool(self.s["ALLOW_DELETE_DATA"])
            if not self.s["MODELS"]:
                self.s["MODELS"] = default_models(self.s["REGION"])
            for k, v in self.s.items():
                bi.set_global_variable("${%s}" % k, v)
        return self.s

    def start_run(self, needs_aws=True):
        """Starts a test run: picks the run id, finds the account and writes the base config.

        Returns the run id. With needs_aws=False (the cli suite) nothing is read from AWS."""
        s = self._settings()
        self.run_id = s["RUN_ID"] or "e2e-" + secrets.token_hex(3)
        out = Path(BuiltIn().get_variable_value("${OUTPUT DIR}"))
        self.work = out / "work"
        self.work.mkdir(parents=True, exist_ok=True)
        # The CLIs run in an empty directory, so a ./bedrock.yaml is never picked up by accident.
        self.cwd = self.work / "cwd"
        self.cwd.mkdir(exist_ok=True)
        self.account = "111122223333"
        self.offline = not _bool(needs_aws)
        if not self.offline:
            ident = self._session("admin").client("sts").get_caller_identity()
            self.account = ident["Account"]
            logger.info(f"Admin: {ident['Arn']}", also_console=True)
        self.cfg = {
            "id": self.run_id,
            "account": self.account,
            "region": s["REGION"],
            "product": s["PRODUCT"],
            "models": list(s["MODELS"]),
            "defaults": {"limit_usd": 1, "notify": s["NOTIFY_EMAIL"] or "owner"},
            "cost_export": {"bucket": self.bucket},
            "block_direct_calls": {"enabled": False},
            "users": [],
        }
        logger.info(f"Run id: {self.run_id}", also_console=True)
        BuiltIn().set_global_variable("${RUN}", self.run_id)
        BuiltIn().set_global_variable("${RUN_CONFIG_ID}", self.run_id)
        BuiltIn().set_global_variable("${ACCOUNT}", self.account)
        return self.run_id

    @property
    def bucket(self):
        return self._settings()["CUR_BUCKET"] or f"bedrock-cur-{self.account}"

    def use_account_id(self):
        """Makes this run own the account setup: id ACCOUNT_ID, and Step 1.7 enabled."""
        s = self._settings()
        self.cfg["id"] = s["ACCOUNT_ID"]
        BuiltIn().set_global_variable("${RUN_CONFIG_ID}", s["ACCOUNT_ID"])
        self.cfg["block_direct_calls"] = {"enabled": True, "method": "permission-set",
                                          "admin_permission_set": s["ADMIN_PERMISSION_SET"]}
        return s["ACCOUNT_ID"]

    # ---------------------------------------------------------------- the config file

    def user_email(self, x):
        """Email of test user x: e2e-<hex>-x@TEST_EMAIL_DOMAIN; "me" is the runner's SSO email."""
        if x == "me":
            if not self.sso_email:
                raise Failure("Start SSO first")
            return self.sso_email
        if "@" in x:
            return x
        return f"{self.run_id}-{x}@{self._settings()['TEST_EMAIL_DOMAIN']}"

    def user_name(self, x):
        """Name of test user x: role bedrock-user-<name>, budget bedrock-<name>."""
        for u in self.cfg["users"]:
            if u["email"] == self.user_email(x) and u.get("name"):
                return u["name"]
        if x == "me" or "@" in x:
            return f"{self.run_id}-{x.split('@')[0] if x != 'me' else 'me'}"
        return f"{self.run_id}-{x}"

    def role_of(self, x):
        return "bedrock-user-" + self.user_name(x)

    def budget_of(self, x):
        return "bedrock-" + self.user_name(x)

    def set_users(self, *xs):
        self.cfg["users"] = []
        for x in xs:
            self.add_user(x)

    def add_user(self, x, **options):
        """Adds test user x. "me" and full emails get name: <run id>-<local part> so they never
        collide with a real user's role."""
        u = {"email": self.user_email(x)}
        if x == "me" or "@" in x:
            u["name"] = self.user_name(x)
        u.update({k: _value(v) for k, v in options.items()})
        self.cfg["users"] = [v for v in self.cfg["users"] if v["email"] != u["email"]] + [u]

    def remove_user(self, x):
        self.cfg["users"] = [u for u in self.cfg["users"] if u["email"] != self.user_email(x)]

    def set_user(self, x, key, value):
        for u in self.cfg["users"]:
            if u["email"] == self.user_email(x):
                u[key] = _value(value)
                return
        raise Failure(f"no user {x}")

    def unset_user(self, x, key):
        for u in self.cfg["users"]:
            if u["email"] == self.user_email(x):
                u.pop(key, None)

    def set_config(self, path, value):
        """Sets a config value by dotted path, for example defaults.pause_at_percent."""
        d = self.cfg
        *parents, last = path.split(".")
        for p in parents:
            d = d.setdefault(p, {})
        d[last] = _value(value)

    def unset_config(self, path):
        d = self.cfg
        *parents, last = path.split(".")
        for p in parents:
            d = d.get(p, {})
        d.pop(last, None)

    def write_config(self, name="bedrock.yaml", cfg=None):
        """Writes the run's config (JSON is valid YAML) and returns its path."""
        path = self.work / name
        path.write_text(json.dumps(cfg or self.cfg, indent=2) + "\n")
        return str(path)

    def write_raw_config(self, text, name="raw.yaml"):
        path = self.work / name
        path.write_text(text)
        return str(path)

    def write_config_with_id(self, id_, *xs, name=None):
        """Writes a copy of the config with another id and users xs (Step 1.7 off); returns its path."""
        cfg = json.loads(json.dumps(self.cfg))
        cfg["id"] = id_
        cfg["block_direct_calls"] = {"enabled": False}
        cfg["users"] = [{"email": self.user_email(x)} for x in xs]
        return self.write_config(name or f"{id_}.yaml", cfg)

    # ---------------------------------------------------------------- running the CLIs

    def _exec(self, cmd, env, stdin, label):
        logger.info("$ " + " ".join(cmd))
        start = time.time()
        p = subprocess.run(cmd, input=stdin, capture_output=True, text=True, env=env, cwd=self.cwd, timeout=1800)
        logger.info(f"exit {p.returncode} after {time.time() - start:.0f}s\n--- stdout\n{p.stdout}\n--- stderr\n{p.stderr}")
        _console(f"    {label} → exit {p.returncode} ({time.time() - start:.0f}s)")
        return Result(p.returncode, p.stdout, p.stderr)

    def _env(self):
        """The CLIs' environment. Offline (the cli suite) it has no AWS credentials at all."""
        env = dict(os.environ)
        if self.offline:
            for k in [k for k in env if k.startswith("AWS_")]:
                del env[k]
            empty = str(self.work / "empty")
            Path(empty).touch()
            env.update({"AWS_CONFIG_FILE": empty, "AWS_SHARED_CREDENTIALS_FILE": empty,
                        "AWS_EC2_METADATA_DISABLED": "true", "HOME": str(self.cwd)})
        return env

    @staticmethod
    def _only_handoffs_pending(stdout):
        """True when a plan/apply --json exit 2 is caused only by org admin handoffs (a linked account)."""
        try:
            d = json.loads(stdout)
        except ValueError:
            # Text output: a "Pending:" section of "  <label>" lines, each followed by "    <reason>".
            lines = stdout.splitlines()
            if "Pending:" not in lines:
                return False
            reasons = []
            for l in lines[lines.index("Pending:") + 1:]:
                if not l.strip() or not l.startswith("  "):
                    break
                if l.startswith("    "):
                    reasons.append(l)
            return bool(reasons) and all("needs org admin" in l for l in reasons)
        pending = d.get("pending") or [i for i in d.get("items", []) if i.get("op") in PENDING_OPS]
        return bool(pending) and all(i.get("op") == "needs-org-admin" for i in pending)

    @staticmethod
    def _check_rc(r, rc, cmd):
        if str(rc) != "any" and r.rc != int(rc):
            raise Failure(f"{cmd}: exit {r.rc}, expected {rc}\n{r.stdout}\n{r.stderr}")

    def admin(self, *args, rc=0, config="", stdin="", output="stdout"):
        """Runs bedrock-admin -f <run config> args. Checks the exit code (rc=any to skip).

        config=none runs it without -f. Returns stdout, parsed JSON when --json is given, or the
        whole result (output=result)."""
        env = self._env()
        if not self.offline:
            env["AWS_PROFILE"] = self._settings()["ADMIN_PROFILE"]
        env.pop("BEDROCK_ADMIN_CONFIG", None)
        cmd = [str(BIN / "bedrock-admin")]
        if config is not None and str(config).lower() != "none":
            cmd += ["-f", config or self.write_config()]
        cmd += [str(a) for a in args]
        r = self._exec(cmd, env, stdin, "bedrock-admin " + " ".join(str(a) for a in args))
        if str(rc) == "0" and r.rc == 2 and self._only_handoffs_pending(r.stdout):
            r = Result(0, r.stdout, r.stderr)
        self._check_rc(r, rc, "bedrock-admin " + " ".join(str(a) for a in args))
        if output == "result":
            return r
        if "--json" in args and output == "stdout":
            try:
                return json.loads(r.stdout)
            except ValueError as e:
                raise Failure(f"not JSON: {e}\n{r.stdout}")
        return r.stdout

    def bedrock(self, *args, rc=0, stdin="", output="stdout"):
        """Runs the user CLI with the run's own AWS config file (see Start SSO)."""
        env = self._env()
        env.pop("AWS_PROFILE", None)
        if self.user_aws_config:
            env["AWS_CONFIG_FILE"] = str(self.user_aws_config)
        r = self._exec([str(BIN / "bedrock")] + [str(a) for a in args], env, stdin, "bedrock " + " ".join(args))
        self._check_rc(r, rc, "bedrock " + " ".join(args))
        if output == "result":
            return r
        if "--json" in args:
            return json.loads(r.stdout)
        return r.stdout

    # ---------------------------------------------------------------- plan --json

    @staticmethod
    def plan_item(plan, target, section=None):
        """The plan item whose target is target (or, failing that, contains it)."""
        items = [i for i in plan["items"] if section is None or i["section"] == section]
        for i in items:
            if i["target"] == target:
                return i
        found = [i for i in items if target in i["target"]]
        if len(found) == 1:
            return found[0]
        raise Failure(f"no single plan item {target!r} in: {[i['target'] for i in items]}")

    def plan_op_should_be(self, plan, target, op):
        i = self.plan_item(plan, target)
        if i["op"] != op:
            raise Failure(f"{target}: op {i['op']} ({i.get('status')}, {i.get('details')}), expected {op}")
        return i

    @staticmethod
    def pending_targets(plan):
        return [i["target"] for i in plan["items"] if i["op"] in PENDING_OPS]

    @staticmethod
    def changes_but_handoffs(plan):
        """Pending targets other than the org admin handoffs (1.6 outside the management account, 1.7)."""
        return [i["target"] for i in plan["items"] if i["op"] in PENDING_OPS and i["op"] != "needs-org-admin"]

    def plan_should_have_no_changes(self, plan):
        p = self.changes_but_handoffs(plan)
        if p:
            raise Failure(f"plan has changes: {p}")

    def only_pending_should_be(self, plan, *targets):
        p = self.pending_targets(plan)
        bad = [t for t in p if not any(x == t or x in t for x in targets)]
        missing = [x for x in targets if not any(x == t or x in t for t in p)]
        if bad or missing:
            raise Failure(f"pending {p}, expected exactly {list(targets)}")

    # ---------------------------------------------------------------- AWS checks

    def _session(self, which):
        s = self._settings()
        profile = {"admin": s["ADMIN_PROFILE"], "sso": s["SSO_PROFILE"],
                   "untracked": s["UNTRACKED_PROFILE"] or s["ADMIN_PROFILE"]}.get(which, which)
        if which not in self._sessions:
            self._sessions[which] = boto3.Session(profile_name=profile or None, region_name=s["REGION"])
        return self._sessions[which]

    def _user_session(self, x):
        """A session in test user x's personal role, assumed from the admin profile with session
        name <email> (needs Let Admin Assume first). Cached for 45 minutes."""
        c = self._user_sessions.get(x)
        if c and c[1] > time.time():
            return c[0]
        email = self.user_email(x)
        arn = f"arn:aws:iam::{self.account}:role/bedrock-users/{self.role_of(x)}"
        sts = self._session("admin").client("sts")
        for attempt in range(12):
            try:
                cr = sts.assume_role(RoleArn=arn, RoleSessionName=email, DurationSeconds=3600)["Credentials"]
                break
            except ClientError as e:
                if attempt == 11 or e.response["Error"]["Code"] != "AccessDenied":
                    raise Failure(f"can't assume {arn} as {email}: {e}")
                time.sleep(5)
        sess = boto3.Session(aws_access_key_id=cr["AccessKeyId"], aws_secret_access_key=cr["SecretAccessKey"],
                             aws_session_token=cr["SessionToken"], region_name=self.s["REGION"])
        self._user_sessions[x] = (sess, time.time() + 45 * 60)
        return sess

    def let_admin_assume(self, x):
        """Test only: adds a trust statement so the admin profile can assume test user x's role with
        session name <email>, to spend as x. The role's owner tag still attributes the spend to x.
        The statement goes away with the role at cleanup (apply does not check the trust policy)."""
        arn = self._session("admin").client("sts").get_caller_identity()["Arn"]
        m = re.match(r"^arn:aws:sts::\d{12}:assumed-role/([^/]+)/", arn)
        principal = self._iam().get_role(RoleName=m.group(1))["Role"]["Arn"] if m else arn
        role = self.role_of(x)
        doc = self._iam().get_role(RoleName=role)["Role"]["AssumeRolePolicyDocument"]
        doc["Statement"] = [st for st in doc["Statement"] if st.get("Sid") != "E2EAdminSpends"] + [{
            "Sid": "E2EAdminSpends", "Effect": "Allow", "Principal": {"AWS": principal}, "Action": "sts:AssumeRole",
            "Condition": {"StringEquals": {"sts:RoleSessionName": self.user_email(x)}}}]
        self._iam().update_assume_role_policy(RoleName=role, PolicyDocument=json.dumps(doc))
        return f"user:{x}"

    def _iam(self):
        return self._session("admin").client("iam")

    def _budgets(self):
        return self._session("admin").client("budgets", region_name="us-east-1")

    def _s3(self):
        return self._session("admin").client("s3", region_name=self._bucket_region())

    def _bucket_region(self):
        c = self._session("admin").client("s3")
        loc = c.get_bucket_location(Bucket=self.bucket).get("LocationConstraint")
        return loc or "us-east-1"

    def role_exists(self, role):
        try:
            self._iam().get_role(RoleName=role)
            return True
        except ClientError as e:
            if e.response["Error"]["Code"] == "NoSuchEntity":
                return False
            raise

    def role_should_exist(self, role):
        if not self.role_exists(role):
            raise Failure(f"role {role} does not exist")

    def role_should_not_exist(self, role):
        if self.role_exists(role):
            raise Failure(f"role {role} still exists")

    def role_tags(self, role):
        tags = self._iam().list_role_tags(RoleName=role)["Tags"]
        return {t["Key"]: t["Value"] for t in tags}

    def role_tag_should_be(self, role, key, value):
        got = self.role_tags(role).get(key)
        if got != value:
            raise Failure(f"{role}: tag {key} is {got!r}, expected {value!r}")

    def role_should_not_have_tag(self, role, key):
        got = self.role_tags(role).get(key)
        if got is not None:
            raise Failure(f"{role}: has tag {key}={got!r}")

    def role_should_have_trust_for(self, role, email):
        doc = json.dumps(self._iam().get_role(RoleName=role)["Role"]["AssumeRolePolicyDocument"])
        if email not in doc:
            raise Failure(f"{role}: trust policy does not name {email}: {doc}")

    def deny_attached(self, role):
        arn = f"arn:aws:iam::{self.account}:policy/bedrock/{DENY_POLICY}"
        pols = self._iam().list_attached_role_policies(RoleName=role)["AttachedPolicies"]
        return any(p["PolicyArn"] == arn for p in pols)

    def deny_policy_should_be_attached(self, role):
        if not self.deny_attached(role):
            raise Failure(f"{DENY_POLICY} is not attached to {role}")

    def deny_policy_should_not_be_attached(self, role):
        if self.deny_attached(role):
            raise Failure(f"{DENY_POLICY} is attached to {role}")

    def budget(self, name):
        try:
            return self._budgets().describe_budget(AccountId=self.account, BudgetName=name)["Budget"]
        except ClientError as e:
            if e.response["Error"]["Code"] == "NotFoundException":
                return None
            raise

    def budget_should_not_exist(self, name):
        if self.budget(name) is not None:
            raise Failure(f"budget {name} still exists")

    def budget_limit_should_be(self, name, usd):
        b = self.budget(name)
        if b is None:
            raise Failure(f"no budget {name}")
        got = float(b["BudgetLimit"]["Amount"])
        if abs(got - float(usd)) > 0.001:
            raise Failure(f"{name}: limit {got}, expected {usd}")

    def budget_alerts_should_be(self, name, *percents):
        ns = self._budgets().describe_notifications_for_budget(AccountId=self.account, BudgetName=name)["Notifications"]
        got = sorted(int(n["Threshold"]) for n in ns)
        want = sorted(int(p) for p in percents)
        if got != want:
            raise Failure(f"{name}: alerts at {got}%, expected {want}%")

    def budget_action(self, name):
        acts = self._budgets().describe_budget_actions_for_budget(AccountId=self.account, BudgetName=name)["Actions"]
        if len(acts) != 1:
            raise Failure(f"{name}: {len(acts)} budget actions, expected 1")
        return acts[0]

    def budget_action_should_be(self, name, threshold=None, status=None):
        a = self.budget_action(name)
        got_t = int(a["ActionThreshold"]["ActionThresholdValue"])
        if threshold is not None and got_t != int(threshold):
            raise Failure(f"{name}: action threshold {got_t}%, expected {threshold}%")
        if status is not None and a["Status"] != status:
            raise Failure(f"{name}: action status {a['Status']}, expected {status}")
        return a

    def budget_action_id(self, name):
        return self.budget_action(name)["ActionId"]

    def pause_state(self, x):
        """The user's pause state, read from the role's tags and the budget action (not the CLI)."""
        if PAUSED_BY_TAG in self.role_tags(self.role_of(x)):
            return "paused-by-hand"
        st = self.budget_action(self.budget_of(x))["Status"]
        return {"STANDBY": "armed", "PENDING": "armed", "EXECUTION_SUCCESS": "paused",
                "REVERSE_SUCCESS": "off"}.get(st, st)

    def pause_states(self, *xs):
        return {x: self.pause_state(x) for x in xs}

    def user_row(self, report, x):
        """The users row of test user x in a usage --json report."""
        email = self.user_email(x).lower()
        rows = [r for r in report["users"] if r["email"].lower() == email]
        if not rows:
            raise Failure(f"no usage row for {email}")
        return rows[0]

    def pause_state_should_be(self, x, state):
        got = self.pause_state(x)
        if got != state:
            raise Failure(f"{x}: pause state {got}, expected {state}")

    def set_budget_limit_directly(self, name, usd):
        """Drift: changes a budget's limit outside the file."""
        b = self.budget(name)
        b["BudgetLimit"]["Amount"] = str(usd)
        for k in ("CalculatedSpend", "LastUpdatedTime", "PlannedBudgetLimits"):
            b.pop(k, None)
        self._budgets().update_budget(AccountId=self.account, NewBudget=b)

    def remove_role_tag(self, role, key):
        self._iam().untag_role(RoleName=role, TagKeys=[key])

    def change_budget_action_directly(self, x):
        """Drift: switches the pause action to manual approval, outside the file."""
        name = self.budget_of(x)
        a = self.budget_action(name)
        self._budgets().update_budget_action(AccountId=self.account, BudgetName=name, ActionId=a["ActionId"],
                                             ApprovalModel="MANUAL")

    def change_lambda_setting_directly(self):
        """Drift: changes the monthly Lambda's SNAPSHOT_PREFIX outside the file."""
        lam = self._session("admin").client("lambda")
        env = lam.get_function_configuration(FunctionName=LAMBDA)["Environment"]["Variables"]
        env["SNAPSHOT_PREFIX"] = "e2e-drift/"
        lam.update_function_configuration(FunctionName=LAMBDA, Environment={"Variables": env})
        lam.get_waiter("function_updated_v2").wait(FunctionName=LAMBDA)

    def lambda_setting(self, key):
        lam = self._session("admin").client("lambda")
        return lam.get_function_configuration(FunctionName=LAMBDA)["Environment"]["Variables"].get(key)

    def lambda_should_exist(self):
        self._session("admin").client("lambda").get_function(FunctionName=LAMBDA)

    def lambda_should_not_exist(self):
        try:
            self._session("admin").client("lambda").get_function(FunctionName=LAMBDA)
        except ClientError as e:
            if e.response["Error"]["Code"] == "ResourceNotFoundException":
                return
            raise
        raise Failure(f"Lambda {LAMBDA} still exists")

    def deny_policy_should_exist(self):
        if not self.account_resource_owner():
            raise Failure(f"no policy {DENY_POLICY}, or it has no {ID_TAG} tag")

    def deny_policy_should_not_exist(self):
        try:
            self._iam().get_policy(PolicyArn=f"arn:aws:iam::{self.account}:policy/bedrock/{DENY_POLICY}")
        except ClientError as e:
            if e.response["Error"]["Code"] == "NoSuchEntity":
                return
            raise
        raise Failure(f"policy {DENY_POLICY} still exists")

    def cost_export_should_exist(self, name="bedrock-cur"):
        c = self._session("admin").client("bcm-data-exports", region_name="us-east-1")
        names = [e["ExportName"] for p in c.get_paginator("list_exports").paginate() for e in p["Exports"]]
        if name not in names:
            raise Failure(f"no cost export {name} (have {names})")

    def cost_allocation_tag_should_be_active(self, key="iamPrincipal/owner"):
        ce = self._session("admin").client("ce", region_name="us-east-1")
        try:
            tags = ce.list_cost_allocation_tags(TagKeys=[key])["CostAllocationTags"]
        except ClientError as e:
            if "linked account" in str(e).lower() or e.response["Error"]["Code"] == "AccessDeniedException":
                raise SkipExecution(f"cost allocation tag {key}: not readable from this account, the org admin "
                                    f"activates it in the management account ({e.response['Error']['Message']})")
            raise
        if not tags or tags[0]["Status"] != "Active":
            raise Failure(f"cost allocation tag {key}: {tags}")

    def personal_roles(self):
        roles = []
        for page in self._iam().get_paginator("list_roles").paginate(PathPrefix="/bedrock-users/"):
            roles += [r["RoleName"] for r in page["Roles"] if r["RoleName"].startswith("bedrock-user-")]
        return roles

    def non_test_personal_roles(self):
        """Personal roles not made by an e2e run: real users."""
        return [r for r in self.personal_roles() if not r.startswith("bedrock-user-e2e-")]

    def create_unmanaged_role(self, x):
        """A personal role made outside bedrock-admin (no id tag). Returns its name."""
        role = self.role_of(x)
        trust = {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": "sts:AssumeRole",
                 "Principal": {"AWS": f"arn:aws:iam::{self.account}:root"}}]}
        self._iam().create_role(RoleName=role, Path="/bedrock-users/", AssumeRolePolicyDocument=json.dumps(trust),
                                Tags=[{"Key": "owner", "Value": self.user_email(x)}])
        return role

    def delete_role(self, role):
        iam = self._iam()
        if not self.role_exists(role):
            return
        for p in iam.list_attached_role_policies(RoleName=role)["AttachedPolicies"]:
            iam.detach_role_policy(RoleName=role, PolicyArn=p["PolicyArn"])
        for n in iam.list_role_policies(RoleName=role)["PolicyNames"]:
            iam.delete_role_policy(RoleName=role, PolicyName=n)
        iam.delete_role(RoleName=role)

    def block_policy_file_should_exist(self):
        """apply's Step 1.7 handoff wrote the policy file for the org admin."""
        path = self.cwd / "bedrock-block-policy.json"
        if not path.exists():
            raise Failure(f"no {path}")
        doc = json.loads(path.read_text())
        if "BedrockOnlyViaPersonalRole" not in json.dumps(doc):
            raise Failure(f"{path} has no BedrockOnlyViaPersonalRole statement")
        return str(path)

    def delete_monthly_schedule(self):
        """A missing piece: deletes the monthly schedule (Step 1.4)."""
        self._session("admin").client("scheduler").delete_schedule(Name=SCHEDULE)

    def schedule_should_exist(self):
        self._session("admin").client("scheduler").get_schedule(Name=SCHEDULE)

    def account_resource_owner(self):
        """The id of the deny policy (the account setup's owner), or "" when there is none."""
        arn = f"arn:aws:iam::{self.account}:policy/bedrock/{DENY_POLICY}"
        try:
            tags = self._iam().list_policy_tags(PolicyArn=arn)["Tags"]
        except ClientError as e:
            if e.response["Error"]["Code"] == "NoSuchEntity":
                return ""
            raise
        return {t["Key"]: t["Value"] for t in tags}.get(ID_TAG, "")

    def bucket_exists(self):
        try:
            self._session("admin").client("s3").head_bucket(Bucket=self.bucket)
            return True
        except ClientError:
            return False

    def s3_object_should_exist(self, key):
        try:
            self._s3().head_object(Bucket=self.bucket, Key=key)
        except ClientError as e:
            raise Failure(f"s3://{self.bucket}/{key}: {e.response['Error']['Code']}")

    def upload_file(self, local, key):
        self._s3().upload_file(str(local), self.bucket, key)
        logger.info(f"uploaded {local} to s3://{self.bucket}/{key}")

    def delete_prefix(self, prefix):
        if not prefix.startswith("e2e/"):
            raise Failure(f"refusing to delete {prefix}: not under e2e/")
        s3 = self._s3()
        for page in s3.get_paginator("list_objects_v2").paginate(Bucket=self.bucket, Prefix=prefix):
            objs = [{"Key": o["Key"]} for o in page.get("Contents", [])]
            if objs:
                s3.delete_objects(Bucket=self.bucket, Delete={"Objects": objs})

    def upload_usage_fixtures(self):
        """Uploads the Parquet fixtures as this and last month's cost export, and the snapshot as
        last month's, under e2e/<run id>/. Returns the path of a config that reads them."""
        now = dt.datetime.now(dt.timezone.utc)
        this = now.strftime("%Y-%m")
        last = (now.replace(day=1) - dt.timedelta(days=1)).strftime("%Y-%m")
        base = f"e2e/{self.run_id}"
        cfg = json.loads(json.dumps(self.cfg))
        cfg["cost_export"].update({"prefix": base + "/cur", "name": "bedrock-cur", "snapshot_prefix": base + "/snapshots"})
        data = f"{base}/cur/bedrock-cur/data"
        self.upload_file(FIXTURES / "cur-this-month.parquet", f"{data}/BILLING_PERIOD={this}/fixture-00001.parquet")
        self.upload_file(FIXTURES / "cur-last-month.parquet", f"{data}/BILLING_PERIOD={last}/fixture-00001.parquet")
        self.upload_file(FIXTURES / "snapshot-last-month.json", f"{base}/snapshots/{last}.json")
        BuiltIn().set_global_variable("${THIS_MONTH}", this)
        BuiltIn().set_global_variable("${LAST_MONTH}", last)
        return self.write_config("usage.yaml", cfg)

    # ---------------------------------------------------------------- the monthly Lambda

    def invoke_monthly_lambda(self):
        """Invokes the monthly Lambda and returns its result. It acts on every user in the account,
        so it only runs when the Lambda is managed by ACCOUNT_ID (a test account)."""
        others = self.non_test_personal_roles()
        if others:
            raise SkipExecution(f"not invoking the monthly Lambda: the account has real users ({', '.join(others[:5])}), "
                                "and it acts on every user (use a test account)")
        lam = self._session("admin").client("lambda")
        tags = lam.list_tags(Resource=f"arn:aws:lambda:{self.s['REGION']}:{self.account}:function:{LAMBDA}")["Tags"]
        if tags.get(ID_TAG) != self.s["ACCOUNT_ID"]:
            raise SkipExecution(f"the Lambda is managed by {tags.get(ID_TAG)!r}, not ACCOUNT_ID {self.s['ACCOUNT_ID']!r}: "
                                "not invoking it, as it acts on every user in the account (use a test account)")
        self._backup_lambda_snapshot()
        r = lam.invoke(FunctionName=LAMBDA, Payload=b"{}")
        body = r["Payload"].read().decode()
        logger.info(f"Lambda result: {body}")
        if r.get("FunctionError"):
            raise Failure(f"Lambda failed: {body}")
        return json.loads(body)

    def _lambda_snapshot_location(self):
        env = self._session("admin").client("lambda").get_function_configuration(FunctionName=LAMBDA)["Environment"]["Variables"]
        now = dt.datetime.now(dt.timezone.utc)
        last = (now.replace(day=1) - dt.timedelta(days=1)).strftime("%Y-%m")
        return env["SNAPSHOT_BUCKET"], f"{env['SNAPSHOT_PREFIX'].strip('/')}/{last}.json"

    def _backup_lambda_snapshot(self):
        """The Lambda overwrites last month's snapshot with the test users; keep what was there."""
        bucket, key = self._lambda_snapshot_location()
        s3 = self._session("admin").client("s3", region_name=self._bucket_region())
        try:
            body = s3.get_object(Bucket=bucket, Key=key)["Body"].read()
        except ClientError as e:
            if e.response["Error"]["Code"] not in ("NoSuchKey", "404"):
                raise
            body = None
        self._snapshot_backup = (bucket, key, body)

    def restore_lambda_snapshot(self):
        """Puts back last month's snapshot as it was before Invoke Monthly Lambda (or removes the
        one the Lambda wrote)."""
        backup = getattr(self, "_snapshot_backup", None)
        if not backup:
            return
        bucket, key, body = backup
        s3 = self._session("admin").client("s3", region_name=self._bucket_region())
        if body is None:
            s3.delete_object(Bucket=bucket, Key=key)
            logger.info(f"removed the test snapshot s3://{bucket}/{key}")
        else:
            s3.put_object(Bucket=bucket, Key=key, Body=body, ContentType="application/json")
            logger.info(f"restored s3://{bucket}/{key}")
        self._snapshot_backup = None

    def lambda_snapshot_should_exist(self, result):
        """The snapshot in the Lambda result was saved where the Lambda's settings say."""
        env = self._session("admin").client("lambda").get_function_configuration(FunctionName=LAMBDA)["Environment"]["Variables"]
        key = f"{env['SNAPSHOT_PREFIX'].strip('/')}/{result['snapshot']['month']}.json"
        try:
            self._session("admin").client("s3", region_name=self._bucket_region()).head_object(Bucket=env["SNAPSHOT_BUCKET"], Key=key)
        except ClientError as e:
            raise Failure(f"no snapshot s3://{env['SNAPSHOT_BUCKET']}/{key}: {e.response['Error']['Code']}")
        return key

    # ---------------------------------------------------------------- SSO and model calls

    def start_sso(self):
        """Checks the runner's SSO session and prepares the run's own AWS config file (a copy of
        ~/.aws/config without any bedrock profile) for the user CLI. Returns the SSO email."""
        s = self._settings()
        if not s["SSO_PROFILE"]:
            raise SkipExecution("SSO_PROFILE is not set in the variable file")
        try:
            arn = self._session("sso").client("sts").get_caller_identity()["Arn"]
        except Exception as e:  # noqa: BLE001 - any failure means: sign in first
            raise Failure(f"no SSO session for {s['SSO_PROFILE']}: run aws sso login --profile {s['SSO_PROFILE']} ({e})")
        m = re.match(r"^arn:aws:sts::(\d{12}):assumed-role/AWSReservedSSO_(.+)_[0-9a-f]+/(.+)$", arn)
        if not m:
            raise Failure(f"{s['SSO_PROFILE']} is not an SSO login: {arn}")
        if m.group(1) != self.account:
            raise Failure(f"{s['SSO_PROFILE']} is in account {m.group(1)}, the admin profile in {self.account}")
        self.sso_permission_set, self.sso_email = m.group(2), m.group(3)
        src = Path(os.environ.get("AWS_CONFIG_FILE", Path.home() / ".aws" / "config"))
        self.user_aws_config = self.work / "aws-config"
        text = src.read_text() if src.exists() else ""
        text = re.sub(r"(?ms)^\[profile bedrock\][^\[]*", "", text)
        self.user_aws_config.write_text(text)
        BuiltIn().set_global_variable("${SSO_EMAIL}", self.sso_email)
        BuiltIn().set_global_variable("${SSO_PROFILE}", s["SSO_PROFILE"])
        BuiltIn().set_global_variable("${USER_AWS_CONFIG}", str(self.user_aws_config))
        logger.info(f"SSO: {arn}", also_console=True)
        return self.sso_email

    def get_sso_permission_set(self):
        return self.sso_permission_set

    def user_aws_config_text(self):
        return self.user_aws_config.read_text()

    def _call(self, profile, model, max_tokens, prompt):
        """One Converse call. profile is admin, sso, untracked, or a profile in the run's AWS config."""
        model = model or self._settings()["MODELS"][0]
        old = os.environ.get("AWS_CONFIG_FILE")
        try:
            if profile.startswith("user:"):
                sess = self._user_session(profile[5:])
            elif profile not in ("admin", "sso", "untracked") and self.user_aws_config:
                os.environ["AWS_CONFIG_FILE"] = str(self.user_aws_config)
                sess = boto3.Session(profile_name=profile)
            else:
                sess = self._session(profile)
            rt = sess.client("bedrock-runtime", region_name=self.s["REGION"])
            r = rt.converse(modelId=model, messages=[{"role": "user", "content": [{"text": prompt}]}],
                            inferenceConfig={"maxTokens": int(max_tokens)})
            u = r["usage"]
            return {"ok": True, "code": "", "message": "", "cost": self._cost(model, u["inputTokens"], u["outputTokens"])}
        except ClientError as e:
            return {"ok": False, "code": e.response["Error"]["Code"], "message": e.response["Error"]["Message"], "cost": 0}
        finally:
            if old is None:
                os.environ.pop("AWS_CONFIG_FILE", None)
            else:
                os.environ["AWS_CONFIG_FILE"] = old

    @staticmethod
    def _cost(model, tin, tout):
        for k, (pin, pout) in PRICES.items():
            if k in model:
                return (tin * pin + tout * pout) / 1e6
        return (tin * 3 + tout * 15) / 1e6

    def model_call_should_work(self, profile, model=None):
        r = self._call(profile, model, 1, "Reply with OK.")
        if not r["ok"]:
            raise Failure(f"call as {profile} failed: {r['code']}: {r['message']}")

    def wait_until_sso_can_assume(self, role_arn, timeout=180):
        """New roles take a while to be assumable (IAM propagation); waits until the SSO login can."""
        sts = self._session("sso").client("sts")
        deadline, err = time.time() + int(timeout), None
        while time.time() < deadline:
            try:
                sts.assume_role(RoleArn=role_arn, RoleSessionName=self.sso_email, DurationSeconds=900)
                return
            except Exception as e:  # noqa: BLE001 - AccessDenied until IAM propagates
                err = e
                time.sleep(5)
        raise Failure(f"SSO login still can't assume {role_arn} after {timeout}s: {err}")

    def model_call_should_be_denied(self, profile, model=None, explicit=True):
        r = self._call(profile, model, 1, "Reply with OK.")
        if r["ok"]:
            raise Failure(f"call as {profile} worked, expected AccessDenied")
        if "AccessDenied" not in r["code"]:
            raise Failure(f"call as {profile}: {r['code']}: {r['message']}, expected AccessDenied")
        if _bool(explicit) and "explicit deny" not in r["message"]:
            raise Failure(f"call as {profile} denied, but not by an explicit deny: {r['message']}")
        return r["message"]

    def model_call_is_denied(self, profile, model=None):
        r = self._call(profile, model, 1, "Reply with OK.")
        return not r["ok"] and "AccessDenied" in r["code"]

    def spend(self, profile, usd, model=None):
        """Makes calls as profile until about usd is spent (estimated from token counts).
        Returns the estimated spend."""
        target, spent, n = float(usd), 0.0, 0
        prompt = "Write a long, detailed essay on the history of bridges. Do not stop early."
        while spent < target:
            r = self._call(profile, model, 4096, prompt)
            if not r["ok"]:
                raise Failure(f"spending as {profile}: {r['code']}: {r['message']} (spent ${spent:.2f})")
            spent, n = spent + r["cost"], n + 1
            if n % 5 == 0:
                _console(f"    spent ${spent:.2f} of ${target:.2f} as {profile}")
        logger.info(f"spent ${spent:.4f} in {n} calls as {profile}", also_console=True)
        return round(spent, 4)

    def identity(self, profile):
        if profile.startswith("user:"):
            return self._user_session(profile[5:]).client("sts").get_caller_identity()["Arn"]
        if profile in ("admin", "sso", "untracked"):
            return self._session(profile).client("sts").get_caller_identity()["Arn"]
        old = os.environ.get("AWS_CONFIG_FILE")
        os.environ["AWS_CONFIG_FILE"] = str(self.user_aws_config)
        try:
            return boto3.Session(profile_name=profile).client("sts").get_caller_identity()["Arn"]
        finally:
            if old is None:
                os.environ.pop("AWS_CONFIG_FILE", None)
            else:
                os.environ["AWS_CONFIG_FILE"] = old

    def write_test_user_profile(self, x, profile=None):
        """Adds a profile for test user x's role to the run's AWS config, with the SSO profile as
        source (the trust policy only lets the SSO login with session name <email> in, so this only
        works for "me"; other users get a profile that the admin profile can't use either)."""
        profile = profile or f"e2e-{x}"
        role = f"arn:aws:iam::{self.account}:role/bedrock-users/{self.role_of(x)}"
        with open(self.user_aws_config, "a") as f:
            f.write(f"\n[profile {profile}]\nrole_arn = {role}\nsource_profile = {self.s['SSO_PROFILE']}\n"
                    f"role_session_name = {self.user_email(x)}\nregion = {self.s['REGION']}\n")
        return profile

    # ---------------------------------------------------------------- questions

    def confirm(self, question):
        """Asks a y/n question on the terminal; skipped when INTERACTIVE is False."""
        if not self._settings()["INTERACTIVE"] or not sys.__stdin__.isatty():
            raise SkipExecution(f"not interactive: {question}")
        _console(f"\n  {question} [y/n] ")
        if sys.__stdin__.readline().strip().lower() not in ("y", "yes"):
            raise Failure(f"answered no: {question}")

    # ---------------------------------------------------------------- billing state

    def _state_path(self, run_id):
        return STATE_DIR / f"{run_id}.json"

    def save_billing_state(self, **values):
        STATE_DIR.mkdir(parents=True, exist_ok=True)
        path = self._state_path(self.run_id)
        st = json.loads(path.read_text()) if path.exists() else {"run_id": self.run_id, "phases": []}
        st.update({k: _value(v) for k, v in values.items()})
        st["config"] = self.cfg
        st["sso_email"] = self.sso_email
        path.write_text(json.dumps(st, indent=2) + "\n")
        return str(path)

    def start_billing_phase1(self):
        st = {"run_id": self.run_id, "phase1_at": dt.datetime.now(dt.timezone.utc).isoformat(), "phases": []}
        STATE_DIR.mkdir(parents=True, exist_ok=True)
        self._state_path(self.run_id).write_text(json.dumps(st))
        return self.save_billing_state()

    def load_billing_state(self):
        """Loads the run given by RUN_ID, else the newest unfinished one, and makes it this run."""
        rid = self._settings()["RUN_ID"]
        if rid:
            path = self._state_path(rid)
            if not path.exists():
                raise Failure(f"no billing state {path}")
        else:
            runs = [p for p in STATE_DIR.glob("*.json") if "phase3" not in json.loads(p.read_text()).get("phases", [])]
            if not runs:
                raise SkipExecution("no unfinished billing run: run ./run.sh billing --include phase1 first")
            path = max(runs, key=lambda p: json.loads(p.read_text())["phase1_at"])
        st = json.loads(path.read_text())
        self.run_id, self.cfg, self.sso_email = st["run_id"], st["config"], st.get("sso_email", "")
        BuiltIn().set_global_variable("${RUN}", self.run_id)
        BuiltIn().set_global_variable("${RUN_CONFIG_ID}", self.cfg["id"])
        logger.info(f"Billing run {self.run_id}, phase 1 at {st['phase1_at']}", also_console=True)
        return st

    def billing_state(self):
        return json.loads(self._state_path(self.run_id).read_text())

    def hours_since_phase1(self):
        t = dt.datetime.fromisoformat(self.billing_state()["phase1_at"])
        return (dt.datetime.now(dt.timezone.utc) - t).total_seconds() / 3600

    def require_hours_since_phase1(self, hours):
        """Skips with "retry after <time>" when phase 1 was less than hours ago."""
        h = self.hours_since_phase1()
        if h < float(hours):
            at = dt.datetime.now(dt.timezone.utc) + dt.timedelta(hours=float(hours) - h)
            raise SkipExecution(f"too early: retry after {at:%Y-%m-%d %H:%M} UTC ({hours} h after phase 1)")

    def billing_caught_up_or_skip(self, ok, what):
        """When ok is false: skips with "retry after <time>", or fails 48 h after phase 1."""
        if _bool(ok):
            return
        h = self.hours_since_phase1()
        if h >= 48:
            raise Failure(f"{what}: still not seen {h:.0f} h after phase 1")
        at = dt.datetime.now(dt.timezone.utc) + dt.timedelta(hours=2)
        raise SkipExecution(f"billing not caught up yet ({what}): retry after {at:%Y-%m-%d %H:%M} UTC")

    def mark_phase_done(self, phase):
        st = self.billing_state()
        st["phases"] = sorted(set(st.get("phases", [])) | {phase})
        self._state_path(self.run_id).write_text(json.dumps(st, indent=2) + "\n")

    def delete_billing_state(self):
        self._state_path(self.run_id).unlink(missing_ok=True)

    # ---------------------------------------------------------------- small helpers

    @staticmethod
    def json_path(data, path):
        """Gets a value by dotted path; list items by index or by key=value, e.g. users.email=a@x.y.stat"""
        cur = data
        for part in re.findall(r"[^.=]+=[^=]+?(?=\.[a-z_]+(?:\.|$)|$)|[^.]+", path):
            if isinstance(cur, list):
                if "=" in part:
                    k, v = part.split("=", 1)
                    m = [i for i in cur if str(i.get(k)) == v]
                    if not m:
                        raise Failure(f"no item with {k}={v} in {cur}")
                    cur = m[0]
                else:
                    cur = cur[int(part)]
            else:
                if part not in cur:
                    raise Failure(f"no key {part!r} in {list(cur)}")
                cur = cur[part]
        return cur

    @staticmethod
    def should_be_close(got, want, tolerance=0.01):
        if abs(float(got) - float(want)) > float(tolerance):
            raise Failure(f"{got} is not within {tolerance} of {want}")

    @staticmethod
    def copy_file(src, dst):
        shutil.copy(src, dst)
