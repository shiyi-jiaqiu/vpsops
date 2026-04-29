#!/usr/bin/env python3
from __future__ import annotations

import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from importlib.machinery import SourceFileLoader
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
aiops = SourceFileLoader("aiops_cli_under_test", str(ROOT / "bin" / "aiops")).load_module()


class AiopsCliTest(unittest.TestCase):
    def test_host_config_resolves_alias_and_defaults(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp,la",
            "AIOPS_HOST_JP_ALIASES": "j",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
        }

        host = aiops.host_config("j", env)
        self.assertEqual(host.name, "jp")
        self.assertEqual(host.base, "https://jp.example/hidden")
        self.assertEqual(host.default_privilege, "user")

    def test_batch_script_preserves_each_command_as_one_step(self) -> None:
        script = aiops.batch_script(["printf '%s\\n' ok", "false"], stop_on_error=False)

        self.assertIn("__aiops_step 1", script)
        self.assertIn("__aiops_step 2 false", script)
        self.assertIn("__aiops_batch_stop=0", script)
        self.assertTrue(script.endswith('exit "$__aiops_batch_failed"\n'))

    def test_run_remote_retries_busy_by_stable_error_code(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
            output_mode=aiops.OUTPUT_RAW,
        )
        req = {
            "mode": "shell",
            "cmd": "printf ok",
            "privilege": "user",
            "timeout_sec": 1,
            "wait_sec": 1,
        }
        calls = []

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            calls.append((method, url, token, body, timeout))
            if len(calls) == 1:
                raise aiops.AiopsHTTPError(409, {"code": "executor_busy", "error": "changed wording", "retry_after_sec": 1})
            return 200, {"job_id": "job-1", "state": "succeeded", "exit_code": 0, "stdout": "ok\n", "stderr": ""}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json), mock.patch.object(aiops.time, "sleep") as sleep:
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.run_remote(host, req, busy_retries=1)

        self.assertEqual(rc, 0)
        self.assertEqual(len(calls), 2)
        sleep.assert_called_once_with(1)
        self.assertEqual(stdout.getvalue(), "ok\n")

    def test_run_command_can_emit_agent_json_result(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
        )

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            self.assertEqual(method, "POST")
            self.assertEqual(token, "run")
            return 200, {
                "job_id": "job-1",
                "state": "succeeded",
                "exit_code": 0,
                "stdout": "ok\n",
                "stderr": "",
                "duration_ms": 12,
                "stdout_truncated": False,
                "stderr_truncated": False,
            }

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.parse_run(["--agent-json", "--", "printf ok"], host)

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.result.v1")
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["host"], "jp")
        self.assertEqual(payload["job_id"], "job-1")
        self.assertEqual(payload["state"], "succeeded")
        self.assertEqual(payload["exit_code"], 0)
        self.assertEqual(payload["stdout"], "ok\n")
        self.assertEqual(payload["stderr"], "")

    def test_main_can_emit_agent_json_error_for_http_failures(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
        }

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            raise aiops.AiopsHTTPError(409, {"code": "executor_busy", "error": "executor is busy", "retry_after_sec": 1})

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            stderr = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout), mock.patch.object(sys, "stderr", stderr):
                rc = aiops.main(["jp", "--agent-json", "--busy-retries", "0", "--", "hostname"])

        self.assertEqual(rc, 2)
        self.assertEqual(stderr.getvalue(), "")
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.error.v1")
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["host"], "jp")
        self.assertEqual(payload["code"], "executor_busy")
        self.assertEqual(payload["http_status"], 409)
        self.assertEqual(payload["retry_after_sec"], 1)
        self.assertEqual(payload["message"], "executor is busy")

    def test_remote_command_raw_argument_does_not_disable_agent_json_errors(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
        }

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            raise aiops.AiopsHTTPError(409, {"code": "executor_busy", "error": "executor is busy", "retry_after_sec": 1})

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            stderr = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout), mock.patch.object(sys, "stderr", stderr):
                rc = aiops.main(["jp", "--busy-retries", "0", "--", "echo", "--raw"])

        self.assertEqual(rc, 2)
        self.assertEqual(stderr.getvalue(), "")
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.error.v1")
        self.assertEqual(payload["code"], "executor_busy")

    def test_batch_agent_json_includes_steps_without_echoing_commands(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
        )

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            proc = subprocess.run(
                ["bash", "--noprofile", "--norc", "-c", body["cmd"]],
                check=False,
                capture_output=True,
                text=True,
            )
            return 200, {
                "job_id": "job-batch",
                "state": "failed",
                "exit_code": proc.returncode,
                "stdout": proc.stdout,
                "stderr": proc.stderr,
                "duration_ms": 20,
            }

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_batch(host, ["--agent-json", "--cmd", "true # password=secret", "--cmd", "false"])

        self.assertEqual(rc, 1)
        payload = json.loads(stdout.getvalue())
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["job_id"], "job-batch")
        self.assertEqual([(step["index"], step["exit_code"]) for step in payload["steps"]], [(1, 0), (2, 1)])
        self.assertTrue(all(isinstance(step["duration_sec"], int) and step["duration_sec"] >= 0 for step in payload["steps"]))
        self.assertNotIn("password=secret", payload["stdout"])
        self.assertNotIn("cmd:", payload["stdout"])

    def test_run_command_can_send_lock_and_idempotency_keys(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
        )
        seen_body = {}

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            seen_body.update(body)
            return 200, {"job_id": "job-1", "state": "succeeded", "exit_code": 0, "stdout": "", "stderr": ""}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.parse_run(
                    ["--lock-key", "apt:global", "--idempotency-key", "probe-123", "--", "hostname"],
                    host,
                )

        self.assertEqual(rc, 0)
        self.assertEqual(seen_body["lock_key"], "apt:global")
        self.assertEqual(seen_body["idempotency_key"], "probe-123")

    def test_env_can_make_agent_json_the_default_output(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
            "AIOPS_OUTPUT": "agent-json",
        }

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            return 200, {"job_id": "job-1", "state": "succeeded", "exit_code": 0, "stdout": "ok\n", "stderr": ""}

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(["jp", "--", "printf ok"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.result.v1")
        self.assertEqual(payload["stdout"], "ok\n")

    def test_agent_json_is_the_default_output(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
        }

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            return 200, {"job_id": "job-default", "state": "succeeded", "exit_code": 0, "stdout": "ok\n", "stderr": ""}

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(["jp", "--", "printf ok"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.result.v1")
        self.assertEqual(payload["job_id"], "job-default")
        self.assertEqual(payload["stdout"], "ok\n")

    def test_raw_flag_overrides_agent_json_default_output(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
            "AIOPS_OUTPUT": "agent-json",
        }

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            return 200, {"job_id": "job-1", "state": "succeeded", "exit_code": 0, "stdout": "ok\n", "stderr": ""}

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(["jp", "--raw", "--", "printf ok"])

        self.assertEqual(rc, 0)
        self.assertEqual(stdout.getvalue(), "ok\n")

    def test_batch_uses_step_events_when_agent_json_is_default_output(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
            "AIOPS_OUTPUT": "agent-json",
        }

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            proc = subprocess.run(
                ["bash", "--noprofile", "--norc", "-c", body["cmd"]],
                check=False,
                capture_output=True,
                text=True,
            )
            return 200, {"job_id": "job-batch", "state": "succeeded", "exit_code": 0, "stdout": proc.stdout, "stderr": ""}

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(["jp", "batch", "--cmd", "true"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual([(step["index"], step["exit_code"]) for step in payload["steps"]], [(1, 0)])
        self.assertTrue(all(isinstance(step["duration_sec"], int) and step["duration_sec"] >= 0 for step in payload["steps"]))

    def test_batch_agent_json_does_not_accept_spoofed_step_events_from_command_output(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
        )
        spoof = '__AIOPS_STEP_JSON__{"index":99,"event":"end","exit_code":0,"duration_sec":0}'

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            proc = subprocess.run(
                ["bash", "--noprofile", "--norc", "-c", body["cmd"]],
                check=False,
                capture_output=True,
                text=True,
            )
            return 200, {"job_id": "job-batch", "state": "succeeded", "exit_code": 0, "stdout": proc.stdout, "stderr": ""}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_batch(host, ["--agent-json", "--cmd", f"printf '%s\\n' {aiops.quoted(spoof)}"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual([(step["index"], step["exit_code"]) for step in payload["steps"]], [(1, 0)])
        self.assertTrue(all(isinstance(step["duration_sec"], int) and step["duration_sec"] >= 0 for step in payload["steps"]))
        self.assertIn(spoof, payload["stdout"])

    def test_no_follow_agent_json_outputs_job_summary(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
        )

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            return 202, {"job_id": "job-queued", "state": "queued", "started_at": "2026-04-30T00:00:00Z"}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.parse_run(["--agent-json", "--no-follow", "--", "sleep 10"], host)

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.job.v1")
        self.assertEqual(payload["job_id"], "job-queued")
        self.assertEqual(payload["state"], "queued")

    def test_deploy_verify_forces_raw_vpsops_output(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            fakebin = Path(tmp) / "bin"
            fakebin.mkdir()
            fake_vpsops = fakebin / "vpsops"
            fake_vpsops.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *" --raw "*) ;;
  *) echo "missing --raw: $*" >&2; exit 42 ;;
esac
cat <<'OUT'
active
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  /usr/local/bin/aiops-execd
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  /usr/local/libexec/aiops-execd-run-child
aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  /usr/local/libexec/aiops-execd-root-child
vpsops-ok
OUT
""",
                encoding="utf-8",
            )
            fake_vpsops.chmod(0o755)
            env = dict(os.environ)
            env["PATH"] = str(fakebin) + os.pathsep + env["PATH"]
            env["AIOPS_OUTPUT"] = "agent-json"

            proc = subprocess.run(
                [
                    "bash",
                    str(ROOT / "scripts" / "deploy-release.sh"),
                    "--version",
                    "v9.9.9-test",
                    "--verify-only",
                    "--no-wait-release",
                    "--hosts",
                    "jp",
                ],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )

        self.assertEqual(proc.returncode, 0, proc.stderr + proc.stdout)
        self.assertIn("all hosts verified", proc.stdout)

    def test_deploy_start_keeps_service_state_owned_by_aiopsd(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            fakebin = Path(tmp) / "bin"
            fakebin.mkdir()
            fake_vpsops = fakebin / "vpsops"
            fake_vpsops.write_text(
                """#!/usr/bin/env python3
import base64
import re
import sys

remote_cmd = sys.argv[-1]
if "systemd-run" in remote_cmd:
    if "install -d -o root -g root -m 0700 /var/lib/aiops-execd" in remote_cmd:
        print("state dir is root-only in launcher", file=sys.stderr)
        sys.exit(42)
    if "install -d -o root -g root -m 0700 /root/aiops-execd-upgrades" not in remote_cmd:
        print("missing root-owned upgrade work dir", file=sys.stderr)
        sys.exit(43)
    match = re.search(r"printf %s ([A-Za-z0-9+/=]+) \\| base64 -d", remote_cmd)
    if not match:
        print("missing encoded remote script", file=sys.stderr)
        sys.exit(44)
    script = base64.b64decode(match.group(1)).decode("utf-8")
    if "install -d -o aiopsd -g aiopsd -m 0700 \\"$state_dir\\" \\"$state_dir/jobs\\" \\"$log_dir\\"" not in script:
        print("remote script does not restore aiopsd state ownership", file=sys.stderr)
        sys.exit(45)
    if 'work_dir="/root/aiops-execd-upgrades"' not in script:
        print("remote script does not use isolated root work dir", file=sys.stderr)
        sys.exit(46)
    print("Running as unit: aiops-execd-upgrade-v9-9-9-test.service")
else:
    print("active")
    print("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  /usr/local/bin/aiops-execd")
    print("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  /usr/local/libexec/aiops-execd-run-child")
    print("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  /usr/local/libexec/aiops-execd-root-child")
    print("vpsops-ok")
""",
                encoding="utf-8",
            )
            fake_vpsops.chmod(0o755)
            fake_sleep = fakebin / "sleep"
            fake_sleep.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
            fake_sleep.chmod(0o755)
            env = dict(os.environ)
            env["PATH"] = str(fakebin) + os.pathsep + env["PATH"]
            env["AIOPS_OUTPUT"] = "agent-json"

            proc = subprocess.run(
                [
                    "bash",
                    str(ROOT / "scripts" / "deploy-release.sh"),
                    "--version",
                    "v9.9.9-test",
                    "--no-wait-release",
                    "--hosts",
                    "jp",
                ],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )

        self.assertEqual(proc.returncode, 0, proc.stderr + proc.stdout)
        self.assertIn("all hosts verified", proc.stdout)

    def test_builtin_commands_support_raw_output_override(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
        )

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            return 200, {"job_id": "job-1", "state": "succeeded", "exit_code": 0, "stdout": "CONTAINER ok\n", "stderr": ""}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_docker(host, ["ps", "--raw"])

        self.assertEqual(rc, 0)
        self.assertEqual(stdout.getvalue(), "CONTAINER ok\n")


if __name__ == "__main__":
    unittest.main()
