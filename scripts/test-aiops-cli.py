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

    def test_hosts_json_reports_configured_hosts_without_tokens(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp,sg",
            "AIOPS_HOST_JP_ALIASES": "j,jp1",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "run-secret",
            "AIOPS_HOST_JP_ROOT_TOKEN": "root-secret",
            "AIOPS_HOST_SG_BASE": "https://sg.example/hidden",
            "AIOPS_HOST_SG_RUN_TOKEN": "sg-run",
            "AIOPS_HOST_SG_ROOT_TOKEN": "sg-root",
            "AIOPS_DEFAULT_PRIVILEGE": "root",
        }

        stdout = io.StringIO()
        with mock.patch.object(sys, "stdout", stdout):
            rc = aiops.command_hosts(env, ["--json"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.hosts.v1")
        self.assertEqual(payload["env_file"], ".env.test")
        self.assertEqual([item["name"] for item in payload["hosts"]], ["jp", "sg"])
        self.assertEqual(payload["hosts"][0]["aliases"], ["j", "jp1"])
        self.assertTrue(payload["hosts"][0]["has_run_token"])
        self.assertTrue(payload["hosts"][0]["has_root_token"])
        self.assertNotIn("run-secret", stdout.getvalue())
        self.assertNotIn("root-secret", stdout.getvalue())

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
        self.assertNotIn("state", payload)
        self.assertNotIn("exit_code", payload)
        self.assertEqual(payload["stdout"], "ok\n")
        self.assertNotIn("stderr", payload)

    def test_success_agent_json_omits_default_empty_fields(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="user",
        )

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            return 200, {
                "job_id": "job-1",
                "state": "succeeded",
                "exit_code": 0,
                "stdout": "ok\n",
                "stderr": "",
                "duration_ms": 12,
                "stdout_truncated": False,
                "stderr_truncated": False,
                "stdout_log_truncated": False,
                "stderr_log_truncated": False,
                "timed_out": False,
                "error": "",
            }

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.parse_run(["--", "printf ok"], host)

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(
            payload,
            {
                "schema": "aiops.cli.result.v1",
                "ok": True,
                "host": "jp",
                "job_id": "job-1",
                "duration_ms": 12,
                "stdout": "ok\n",
            },
        )

    def test_output_profile_agent_full_preserves_success_diagnostics(self) -> None:
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
            return 200, {
                "job_id": "job-full",
                "state": "succeeded",
                "exit_code": 0,
                "stdout": "ok\n",
                "stderr": "",
                "duration_ms": 12,
                "stdout_truncated": False,
                "stderr_truncated": False,
            }

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(["jp", "--output", "agent-full", "--", "printf ok"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.result.v1")
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["state"], "succeeded")
        self.assertEqual(payload["exit_code"], 0)
        self.assertEqual(payload["stderr"], "")
        self.assertFalse(payload["stdout_truncated"])

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

    def test_agent_error_classifies_transport_failures(self) -> None:
        payload = aiops.agent_error_payload("jp", aiops.AiopsTransportError("connection_refused", "request failed: refused"))

        self.assertEqual(payload["schema"], "aiops.cli.error.v1")
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["host"], "jp")
        self.assertEqual(payload["code"], "connection_refused")
        self.assertEqual(payload["http_status"], 0)

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

    def test_batch_agent_json_keeps_steps_when_command_output_has_no_newline(self) -> None:
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
            return 200, {"job_id": "job-batch", "state": "succeeded", "exit_code": 0, "stdout": proc.stdout, "stderr": ""}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_batch(host, ["--agent-json", "--cmd", "printf one", "--cmd", "printf two"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["stdout"], "onetwo")
        self.assertEqual([(step["index"], step["exit_code"]) for step in payload["steps"]], [(1, 0), (2, 0)])
        self.assertNotIn("__AIOPS_STEP_JSON__", payload["stdout"])

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

    def test_job_cancel_posts_cancel_endpoint(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="root",
        )
        seen = {}

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            seen.update({"method": method, "url": url, "token": token})
            return 200, {"job_id": "job-queued", "state": "canceled", "started_at": "2026-05-05T00:00:00Z"}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_job(host, ["cancel", "job-queued"])

        self.assertEqual(rc, 0)
        self.assertEqual(seen["method"], "POST")
        self.assertTrue(seen["url"].endswith("/v1/jobs/job-queued/cancel"))
        self.assertEqual(seen["token"], "root")
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.job.v1")
        self.assertEqual(payload["state"], "canceled")

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

    def test_service_restart_aiops_execd_uses_delayed_self_restart(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="root",
        )
        seen_body = {}

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            seen_body.update(body)
            return 200, {"job_id": "job-1", "state": "succeeded", "exit_code": 0, "stdout": "scheduled\n", "stderr": ""}

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_service(host, ["restart", "aiops-execd"])

        self.assertEqual(rc, 0)
        self.assertIn("systemd-run", seen_body["cmd"])
        self.assertIn("--on-active=2s", seen_body["cmd"])
        self.assertIn("/bin/systemctl restart aiops-execd", seen_body["cmd"])
        self.assertNotIn("systemctl restart aiops-execd && systemctl is-active aiops-execd", seen_body["cmd"])

    def test_remote_doctor_runs_daemon_doctor_probe(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="root",
        )
        seen_body = {}

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            seen_body.update(body)
            return 200, {
                "job_id": "job-doctor",
                "state": "succeeded",
                "exit_code": 0,
                "stdout": "[PASS] config: loaded /etc/aiops-execd/config.json\n",
                "stderr": "",
                "duration_ms": 12,
            }

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_doctor(host, [])

        self.assertEqual(rc, 0)
        self.assertIn("aiops-execd -doctor -doctor-probe", seen_body["cmd"])
        self.assertEqual(seen_body["lock_key"], "aiops:doctor")
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.result.v1")
        self.assertTrue(payload["ok"])
        self.assertIn("[PASS] config", payload["stdout"])

    def test_ops_ssh_surface_checks_effective_port_listeners_and_ufw(self) -> None:
        host = aiops.HostConfig(
            name="jp",
            base="https://jp.example/hidden",
            run_token="run",
            root_token="root",
            default_privilege="root",
        )
        seen_body = {}

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            seen_body.update(body)
            return 200, {
                "job_id": "job-ssh-surface",
                "state": "succeeded",
                "exit_code": 0,
                "stdout": "port 2222\n",
                "stderr": "",
                "duration_ms": 12,
            }

        with mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.command_ops(host, ["ssh-surface"])

        self.assertEqual(rc, 0)
        self.assertIn("sshd -T", seen_body["cmd"])
        self.assertIn("sport = :22 or sport = :2222 or sport = :22022", seen_body["cmd"])
        self.assertIn("ufw status numbered", seen_body["cmd"])
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.result.v1")
        self.assertTrue(payload["ok"])

    def test_fleet_command_returns_one_agent_json_summary(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp,sg",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "jp-run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "jp-root",
            "AIOPS_HOST_SG_BASE": "https://sg.example/hidden",
            "AIOPS_HOST_SG_RUN_TOKEN": "sg-run",
            "AIOPS_HOST_SG_ROOT_TOKEN": "sg-root",
            "AIOPS_DEFAULT_PRIVILEGE": "user",
        }

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            self.assertEqual(method, "POST")
            self.assertEqual(body["cmd"], "hostname")
            if "jp.example" in url:
                return 200, {"job_id": "job-jp", "state": "succeeded", "exit_code": 0, "stdout": "jp\n", "stderr": "", "duration_ms": 10}
            if "sg.example" in url:
                return 200, {"job_id": "job-sg", "state": "succeeded", "exit_code": 0, "stdout": "sg\n", "stderr": "", "duration_ms": 20}
            raise AssertionError(f"unexpected url: {url}")

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(["fleet", "--hosts", "jp,sg", "--parallel", "2", "--", "hostname"])

        self.assertEqual(rc, 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.fleet.v1")
        self.assertTrue(payload["ok"])
        self.assertEqual([item["host"] for item in payload["results"]], ["jp", "sg"])
        self.assertEqual([item["stdout"] for item in payload["results"]], ["jp\n", "sg\n"])
        self.assertNotIn("exit_code", payload["results"][0])

    def test_fleet_plan_runs_precheck_apply_postcheck_with_lock(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp,sg",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "jp-run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "jp-root",
            "AIOPS_HOST_SG_BASE": "https://sg.example/hidden",
            "AIOPS_HOST_SG_RUN_TOKEN": "sg-run",
            "AIOPS_HOST_SG_ROOT_TOKEN": "sg-root",
            "AIOPS_DEFAULT_PRIVILEGE": "root",
        }
        calls = []

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            host = "jp" if "jp.example" in url else "sg"
            calls.append((host, body["cmd"], body.get("lock_key", ""), token))
            return 200, {
                "job_id": f"job-{host}-{len(calls)}",
                "state": "succeeded",
                "exit_code": 0,
                "stdout": f"{host}:{body['cmd']}\n",
                "stderr": "",
                "duration_ms": 10,
            }

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(
                    [
                        "fleet-plan",
                        "--hosts",
                        "jp,sg",
                        "--precheck",
                        "test-pre",
                        "--apply",
                        "test-apply",
                        "--postcheck",
                        "test-post",
                    ]
                )

        self.assertEqual(rc, 0)
        self.assertEqual(
            [(host, cmd, lock) for host, cmd, lock, _token in calls],
            [
                ("jp", "test-pre", ""),
                ("jp", "test-apply", "fleet-plan:default"),
                ("jp", "test-post", ""),
                ("sg", "test-pre", ""),
                ("sg", "test-apply", "fleet-plan:default"),
                ("sg", "test-post", ""),
            ],
        )
        self.assertTrue(all(token.endswith("-root") for *_rest, token in calls))
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema"], "aiops.cli.fleet_plan.v1")
        self.assertTrue(payload["ok"])
        self.assertEqual([item["host"] for item in payload["hosts"]], ["jp", "sg"])
        self.assertEqual([step["phase"] for step in payload["hosts"][0]["steps"]], ["precheck", "apply", "postcheck"])

    def test_fleet_plan_stops_before_apply_when_precheck_fails(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp,sg",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "jp-run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "jp-root",
            "AIOPS_HOST_SG_BASE": "https://sg.example/hidden",
            "AIOPS_HOST_SG_RUN_TOKEN": "sg-run",
            "AIOPS_HOST_SG_ROOT_TOKEN": "sg-root",
            "AIOPS_DEFAULT_PRIVILEGE": "root",
        }
        calls = []

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            host = "jp" if "jp.example" in url else "sg"
            calls.append((host, body["cmd"]))
            return 200, {
                "job_id": "job-failed-precheck",
                "state": "failed",
                "exit_code": 7,
                "stdout": "",
                "stderr": "precheck failed\n",
                "duration_ms": 10,
            }

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(["fleet-plan", "--hosts", "jp,sg", "--precheck", "test-pre", "--apply", "test-apply"])

        self.assertEqual(rc, 7)
        self.assertEqual(calls, [("jp", "test-pre")])
        payload = json.loads(stdout.getvalue())
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["hosts"][0]["host"], "jp")
        self.assertFalse(payload["hosts"][0]["ok"])
        self.assertEqual([step["phase"] for step in payload["hosts"][0]["steps"]], ["precheck"])

    def test_fleet_plan_honors_run_control_options(self) -> None:
        env = {
            "AIOPS_ENV_FILE_RESOLVED": ".env.test",
            "AIOPS_DEFAULT_HOST": "jp",
            "AIOPS_HOSTS": "jp",
            "AIOPS_HOST_JP_BASE": "https://jp.example/hidden",
            "AIOPS_HOST_JP_RUN_TOKEN": "jp-run",
            "AIOPS_HOST_JP_ROOT_TOKEN": "jp-root",
            "AIOPS_DEFAULT_PRIVILEGE": "root",
        }
        seen_bodies = []

        def fake_http_json(method, url, token=None, body=None, timeout=30):
            seen_bodies.append(dict(body))
            return 200, {
                "job_id": "job-jp",
                "state": "succeeded",
                "exit_code": 0,
                "stdout": "",
                "stderr": "",
                "duration_ms": 10,
            }

        with mock.patch.object(aiops, "merged_env", return_value=env), mock.patch.object(aiops, "http_json", side_effect=fake_http_json):
            stdout = io.StringIO()
            with mock.patch.object(sys, "stdout", stdout):
                rc = aiops.main(
                    [
                        "fleet-plan",
                        "--hosts",
                        "jp",
                        "--apply",
                        "test-apply",
                        "--timeout",
                        "45",
                        "--wait",
                        "12",
                        "--kill-grace",
                        "8",
                        "--max-stdout-bytes",
                        "1234",
                        "--max-stderr-bytes",
                        "2345",
                        "--max-stdout-log-bytes",
                        "3456",
                        "--max-stderr-log-bytes",
                        "4567",
                        "--idempotency-key",
                        "fleet-plan-test-1",
                    ]
                )

        self.assertEqual(rc, 0)
        self.assertEqual(len(seen_bodies), 1)
        self.assertEqual(seen_bodies[0]["timeout_sec"], 45)
        self.assertEqual(seen_bodies[0]["wait_sec"], 12)
        self.assertEqual(seen_bodies[0]["kill_grace_sec"], 8)
        self.assertEqual(seen_bodies[0]["max_stdout_bytes"], 1234)
        self.assertEqual(seen_bodies[0]["max_stderr_bytes"], 2345)
        self.assertEqual(seen_bodies[0]["max_stdout_log_bytes"], 3456)
        self.assertEqual(seen_bodies[0]["max_stderr_log_bytes"], 4567)
        self.assertEqual(seen_bodies[0]["idempotency_key"], "fleet-plan-test-1")


if __name__ == "__main__":
    unittest.main()
