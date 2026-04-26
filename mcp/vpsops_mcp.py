#!/usr/bin/env python3
"""Minimal stdio MCP server for the local aiops/vpsops CLI.

This adapter intentionally does not talk to the HTTPS Exec API directly. It
delegates to bin/aiops so token loading, host aliases, busy retry, job polling,
and JSON result handling stay in one place.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import traceback
from pathlib import Path
from typing import Any


PROTOCOL_VERSION = "2025-11-25"
SERVER_NAME = "vpsops"
SERVER_VERSION = "0.1.0"

AIopsRoot = Path(__file__).resolve().parents[1]
AIopsCLI = AIopsRoot / "bin" / "aiops"


class ToolError(Exception):
    pass


def json_response(message_id: Any, result: dict[str, Any]) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": message_id, "result": result}


def json_error(message_id: Any, code: int, message: str, data: Any | None = None) -> dict[str, Any]:
    error: dict[str, Any] = {"code": code, "message": message}
    if data is not None:
        error["data"] = data
    return {"jsonrpc": "2.0", "id": message_id, "error": error}


def write_message(message: dict[str, Any]) -> None:
    sys.stdout.write(json.dumps(message, ensure_ascii=False, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def text_result(text: str, *, is_error: bool = False, structured: dict[str, Any] | None = None) -> dict[str, Any]:
    result: dict[str, Any] = {
        "content": [{"type": "text", "text": text}],
    }
    if is_error:
        result["isError"] = True
    if structured is not None:
        result["structuredContent"] = structured
    return result


def require_str(args: dict[str, Any], name: str, default: str | None = None) -> str:
    value = args.get(name, default)
    if not isinstance(value, str) or not value:
        raise ToolError(f"{name} must be a non-empty string")
    return value


def optional_str(args: dict[str, Any], name: str, default: str = "") -> str:
    value = args.get(name, default)
    if value is None:
        return default
    if not isinstance(value, str):
        raise ToolError(f"{name} must be a string")
    return value


def optional_int(args: dict[str, Any], name: str, default: int, minimum: int, maximum: int) -> int:
    value = args.get(name, default)
    if isinstance(value, bool) or not isinstance(value, int):
        raise ToolError(f"{name} must be an integer")
    if value < minimum or value > maximum:
        raise ToolError(f"{name} must be between {minimum} and {maximum}")
    return value


def optional_bool(args: dict[str, Any], name: str, default: bool = False) -> bool:
    value = args.get(name, default)
    if not isinstance(value, bool):
        raise ToolError(f"{name} must be a boolean")
    return value


def privilege_args(args: dict[str, Any]) -> list[str]:
    privilege = args.get("privilege", "")
    if privilege in {"", None}:
        return []
    if privilege == "root":
        return ["--root"]
    if privilege == "user":
        return ["--user"]
    raise ToolError("privilege must be root or user")


def run_cli(argv: list[str], *, timeout: int) -> dict[str, Any]:
    if not AIopsCLI.exists():
        raise ToolError(f"aiops CLI not found at {AIopsCLI}")
    env = dict(os.environ)
    env.setdefault("PYTHONUNBUFFERED", "1")
    try:
        proc = subprocess.run(
            [str(AIopsCLI), *argv],
            cwd=str(AIopsRoot),
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        return {
            "ok": False,
            "process_exit_code": 124,
            "stdout": exc.stdout or "",
            "stderr": (exc.stderr or "") + f"\n[vpsops-mcp] local timeout after {timeout}s\n",
        }
    return {
        "ok": proc.returncode == 0,
        "process_exit_code": proc.returncode,
        "stdout": proc.stdout,
        "stderr": proc.stderr,
    }


def parse_json_stdout(cli_result: dict[str, Any]) -> dict[str, Any] | None:
    stdout = cli_result.get("stdout") or ""
    if not stdout.strip():
        return None
    try:
        return json.loads(stdout)
    except json.JSONDecodeError:
        return None


def cli_summary(cli_result: dict[str, Any]) -> dict[str, Any]:
    return {
        "ok": bool(cli_result.get("ok")),
        "process_exit_code": cli_result.get("process_exit_code"),
    }


def compact_payload(payload: dict[str, Any]) -> dict[str, Any]:
    keys = [
        "job_id",
        "state",
        "exit_code",
        "signal",
        "timed_out",
        "stdout_truncated",
        "stderr_truncated",
        "stdout_log_truncated",
        "stderr_log_truncated",
        "duration_ms",
        "started_at",
        "finished_at",
        "error",
    ]
    return {key: payload[key] for key in keys if key in payload}


def command_text(payload: dict[str, Any] | None, cli_result: dict[str, Any]) -> str:
    if payload:
        stdout = payload.get("stdout") or ""
        stderr = payload.get("stderr") or ""
        summary = (
            f"job_id={payload.get('job_id')} "
            f"state={payload.get('state')} "
            f"exit_code={payload.get('exit_code')} "
            f"duration_ms={payload.get('duration_ms')}"
        )
        parts = []
        if stdout:
            parts.append(stdout.rstrip("\n"))
        if stderr:
            parts.append("[stderr]\n" + stderr.rstrip("\n"))
        if parts:
            parts.append(summary)
            return "\n".join(parts)
        return summary

    parts = []
    if cli_result.get("stdout"):
        parts.append(str(cli_result["stdout"]).rstrip("\n"))
    if cli_result.get("stderr"):
        parts.append("[stderr]\n" + str(cli_result["stderr"]).rstrip("\n"))
    if not parts:
        parts.append(f"process_exit_code={cli_result.get('process_exit_code')}")
    return "\n".join(parts)


def command_is_error(payload: dict[str, Any] | None, cli_result: dict[str, Any]) -> bool:
    if payload:
        exit_code = payload.get("exit_code")
        try:
            exit_code_int = int(exit_code)
        except (TypeError, ValueError):
            exit_code_int = 1
        return payload.get("state") != "succeeded" or exit_code_int != 0
    return not bool(cli_result.get("ok"))


def run_json_tool(argv: list[str], *, timeout: int) -> dict[str, Any]:
    cli_result = run_cli(argv, timeout=timeout)
    payload = parse_json_stdout(cli_result)
    structured = {"cli": cli_summary(cli_result)}
    if payload is not None:
        structured["result"] = compact_payload(payload)
    return text_result(
        command_text(payload, cli_result),
        is_error=command_is_error(payload, cli_result),
        structured=structured,
    )


def tool_vps_hosts(args: dict[str, Any]) -> dict[str, Any]:
    cli_result = run_cli(["hosts"], timeout=20)
    hosts = []
    for raw in (cli_result.get("stdout") or "").splitlines():
        line = raw.strip()
        if line:
            hosts.append(line)
    return text_result(
        "\n".join(hosts) if hosts else (cli_result.get("stderr") or "no hosts configured"),
        is_error=not cli_result["ok"],
        structured={"hosts": hosts, "cli": cli_summary(cli_result)},
    )


def tool_vps_health(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    cli_result = run_cli([host, "health"], timeout=30)
    payload = parse_json_stdout(cli_result)
    return text_result(
        json.dumps(payload, ensure_ascii=False) if payload is not None else command_text(None, cli_result),
        is_error=not cli_result["ok"],
        structured={"health": payload, "cli": cli_summary(cli_result)},
    )


def tool_vps_run(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    cmd = require_str(args, "cmd")
    cwd = optional_str(args, "cwd", "")
    timeout_sec = optional_int(args, "timeout_sec", 60, 1, 3600)
    wait_sec = optional_int(args, "wait_sec", min(25, timeout_sec), 1, min(300, timeout_sec))
    argv = [host, *privilege_args(args), "--timeout", str(timeout_sec), "--wait", str(wait_sec), "--json"]
    if cwd:
        argv.extend(["--cwd", cwd])
    argv.extend(["--", cmd])
    return run_json_tool(argv, timeout=timeout_sec + 90)


def tool_vps_batch(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    commands = args.get("commands")
    if not isinstance(commands, list) or not commands or not all(isinstance(item, str) and item for item in commands):
        raise ToolError("commands must be a non-empty array of strings")
    timeout_sec = optional_int(args, "timeout_sec", max(120, min(600, 60 * len(commands))), 1, 7200)
    wait_sec = optional_int(args, "wait_sec", min(25, timeout_sec), 1, min(300, timeout_sec))
    argv = [
        host,
        "batch",
        *privilege_args(args),
        "--timeout",
        str(timeout_sec),
        "--wait",
        str(wait_sec),
        "--json",
    ]
    if optional_bool(args, "stop_on_error", False):
        argv.append("--stop-on-error")
    cwd = optional_str(args, "cwd", "")
    if cwd:
        argv.extend(["--cwd", cwd])
    for command in commands:
        argv.extend(["--cmd", command])
    return run_json_tool(argv, timeout=timeout_sec + 90)


def tool_vps_inspect(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    return run_json_tool([host, "inspect", "--json"], timeout=90)


def tool_docker_ps(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    argv = [host, "docker", "ps", "--json"]
    if optional_bool(args, "all", False):
        argv.insert(-1, "--all")
    return run_json_tool(argv, timeout=90)


def tool_docker_logs(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    container = require_str(args, "container")
    tail = optional_int(args, "tail", 200, 1, 5000)
    return run_json_tool([host, "docker", "logs", container, "--tail", str(tail), "--json"], timeout=120)


def tool_service_status(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    name = require_str(args, "name")
    return run_json_tool([host, "service", "status", name, "--json"], timeout=90)


def tool_file_read(args: dict[str, Any]) -> dict[str, Any]:
    host = require_str(args, "host")
    path = require_str(args, "path")
    max_bytes = optional_int(args, "max_bytes", 20000, 1, 262144)
    return run_json_tool([host, "file", "read", path, "--max-bytes", str(max_bytes), "--json"], timeout=90)


def schema_string(description: str, default: str | None = None) -> dict[str, Any]:
    schema: dict[str, Any] = {"type": "string", "description": description}
    if default is not None:
        schema["default"] = default
    return schema


def schema_int(description: str, default: int, minimum: int, maximum: int) -> dict[str, Any]:
    return {"type": "integer", "description": description, "default": default, "minimum": minimum, "maximum": maximum}


def tool_definitions() -> list[dict[str, Any]]:
    host_prop = schema_string("Configured aiops host name or alias, for example prod, web-1, or default.")
    privilege_prop = {
        "type": "string",
        "description": "Execution privilege. Omit to use the host default.",
        "enum": ["root", "user"],
    }
    return [
        {
            "name": "vps_hosts",
            "description": "List locally configured VPS hosts known to the aiops CLI.",
            "inputSchema": {"type": "object", "properties": {}, "additionalProperties": False},
            "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        },
        {
            "name": "vps_health",
            "description": "Check the remote aiops-execd health endpoint for one host.",
            "inputSchema": {
                "type": "object",
                "properties": {"host": host_prop},
                "required": ["host"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        },
        {
            "name": "vps_run",
            "description": "Run one shell command on a VPS through aiops-execd. This is the universal fallback tool.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "host": host_prop,
                    "cmd": schema_string("Shell command to execute remotely."),
                    "privilege": privilege_prop,
                    "cwd": schema_string("Remote working directory. Empty uses server default.", ""),
                    "timeout_sec": schema_int("Remote command timeout in seconds.", 60, 1, 3600),
                    "wait_sec": schema_int("Initial HTTP wait time before async polling.", 25, 1, 300),
                },
                "required": ["host", "cmd"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": False, "destructiveHint": True, "openWorldHint": True},
        },
        {
            "name": "vps_batch",
            "description": "Run multiple shell commands sequentially inside one remote job. Prefer this for multi-step inspection.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "host": host_prop,
                    "commands": {
                        "type": "array",
                        "description": "Shell commands to execute sequentially.",
                        "items": {"type": "string"},
                        "minItems": 1,
                    },
                    "privilege": privilege_prop,
                    "cwd": schema_string("Remote working directory. Empty uses server default.", ""),
                    "timeout_sec": schema_int("Total remote timeout in seconds.", 120, 1, 7200),
                    "wait_sec": schema_int("Initial HTTP wait time before async polling.", 25, 1, 300),
                    "stop_on_error": {"type": "boolean", "description": "Stop after the first failed step.", "default": False},
                },
                "required": ["host", "commands"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": False, "destructiveHint": True, "openWorldHint": True},
        },
        {
            "name": "vps_inspect",
            "description": "Run the standard host/memory/disk/docker/service inspection bundle.",
            "inputSchema": {
                "type": "object",
                "properties": {"host": host_prop},
                "required": ["host"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        },
        {
            "name": "docker_ps",
            "description": "List Docker containers on a VPS.",
            "inputSchema": {
                "type": "object",
                "properties": {"host": host_prop, "all": {"type": "boolean", "default": False}},
                "required": ["host"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        },
        {
            "name": "docker_logs",
            "description": "Read recent Docker logs for one container.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "host": host_prop,
                    "container": schema_string("Container name."),
                    "tail": schema_int("Number of log lines.", 200, 1, 5000),
                },
                "required": ["host", "container"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        },
        {
            "name": "service_status",
            "description": "Read systemd status for one service using --no-pager.",
            "inputSchema": {
                "type": "object",
                "properties": {"host": host_prop, "name": schema_string("systemd unit name.")},
                "required": ["host", "name"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        },
        {
            "name": "file_read",
            "description": "Read a bounded prefix of an absolute remote file path.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "host": host_prop,
                    "path": schema_string("Absolute file path."),
                    "max_bytes": schema_int("Maximum bytes to read.", 20000, 1, 262144),
                },
                "required": ["host", "path"],
                "additionalProperties": False,
            },
            "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": True},
        },
    ]


TOOLS = {
    "vps_hosts": tool_vps_hosts,
    "vps_health": tool_vps_health,
    "vps_run": tool_vps_run,
    "vps_batch": tool_vps_batch,
    "vps_inspect": tool_vps_inspect,
    "docker_ps": tool_docker_ps,
    "docker_logs": tool_docker_logs,
    "service_status": tool_service_status,
    "file_read": tool_file_read,
}


def handle_request(message: dict[str, Any]) -> dict[str, Any] | None:
    method = message.get("method")
    message_id = message.get("id")

    if message_id is None:
        return None

    if method == "initialize":
        params = message.get("params") or {}
        requested_version = params.get("protocolVersion")
        return json_response(
            message_id,
            {
                "protocolVersion": requested_version if isinstance(requested_version, str) else PROTOCOL_VERSION,
                "capabilities": {
                    "tools": {"listChanged": False},
                    "resources": {},
                    "prompts": {},
                },
                "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
            },
        )
    if method == "tools/list":
        return json_response(message_id, {"tools": tool_definitions()})
    if method == "ping":
        return json_response(message_id, {})
    if method == "tools/call":
        params = message.get("params") or {}
        name = params.get("name")
        args = params.get("arguments") or {}
        if not isinstance(name, str) or name not in TOOLS:
            return json_error(message_id, -32602, f"unknown tool: {name}")
        if not isinstance(args, dict):
            return json_error(message_id, -32602, "tool arguments must be an object")
        try:
            return json_response(message_id, TOOLS[name](args))
        except ToolError as exc:
            return json_response(message_id, text_result(str(exc), is_error=True))
    if method in {"resources/list", "prompts/list"}:
        key = "resources" if method == "resources/list" else "prompts"
        return json_response(message_id, {key: []})

    return json_error(message_id, -32601, f"method not found: {method}")


def main() -> int:
    for raw in sys.stdin:
        line = raw.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
            if not isinstance(message, dict):
                write_message(json_error(None, -32700, "message must be a JSON object"))
                continue
            response = handle_request(message)
            if response is not None:
                write_message(response)
        except json.JSONDecodeError as exc:
            write_message(json_error(None, -32700, "parse error", str(exc)))
        except Exception as exc:  # Keep stdio server alive after unexpected failures.
            traceback.print_exc(file=sys.stderr)
            message_id = None
            try:
                message_id = json.loads(line).get("id")
            except Exception:
                pass
            write_message(json_error(message_id, -32603, "internal error", str(exc)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
