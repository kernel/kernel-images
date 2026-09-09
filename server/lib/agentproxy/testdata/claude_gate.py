"""Bounded real-provider gate for a fresh packaged image with ANTHROPIC_API_KEY.
Run with AGENT_API_URL and the pinned runtime/acp/requirements.txt environment.
Native SDK sessions have a $0.15 budget and at most three turns per prompt.
"""
import asyncio
import base64
import json
import os
from pathlib import Path
import tempfile
import urllib.error
import urllib.request
import uuid

import websockets

os.umask(0o077)
base = os.environ["AGENT_API_URL"].rstrip("/")
workspace = "/tmp/claude-gate-" + uuid.uuid4().hex
config_path = "/agent/v1/harnesses/claude/config"
python = "/opt/kernel-agent/venv/bin/python"
evidence = Path(tempfile.mkdtemp(prefix="claude-gate-"))


def http(method, path, data=None, revision=None):
    headers = {"Content-Type": "application/json"}
    if revision is not None:
        headers["If-Match"] = '"' + revision + '"'
    request = urllib.request.Request(base + path, method=method, headers=headers,
        data=json.dumps(data).encode() if data is not None else None)
    try:
        with urllib.request.urlopen(request, timeout=200) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()


def execute(code):
    status, result = http("POST", "/process/exec", {"command": python, "args": ["-c", code]})
    assert status == 200 and result["exit_code"] == 0, "remote fixture command failed"
    return base64.b64decode(result["stdout_b64"]).decode()


def pids():
    # Record the entire connection-owned process groups, not just the adapter.
    return set(json.loads(execute('''import os,pathlib,json
processes={}
for path in pathlib.Path('/proc').iterdir():
 if not path.name.isdigit():continue
 try:
  args=(path/'cmdline').read_bytes().split(bytes([0]))
  processes[int(path.name)]=(os.getpgid(int(path.name)),args)
 except (FileNotFoundError,ProcessLookupError,PermissionError):continue
groups={pgid for pgid,args in processes.values() if b'/opt/kernel-agent/claude/node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js' in args}
print(json.dumps([pid for pid,(pgid,args) in processes.items() if pgid in groups]))
''')))


class Client:
    def __init__(self):
        self.seq = 0
        self.events = []

    async def open(self):
        self.ws = await websockets.connect("ws" + base[4:] + "/agent/v1/acp?harness=claude", max_size=1 << 20)
        try:
            result, _ = await self.call("initialize", {"protocolVersion": 1, "clientCapabilities": {}})
            assert result["agentInfo"]["version"] == "0.75.1"
            assert result["agentCapabilities"]["loadSession"]
        except BaseException:
            await self.ws.close()
            raise
        return self

    async def close(self):
        await self.ws.close()

    async def call(self, method, params):
        self.seq += 1
        await self.ws.send(json.dumps({"jsonrpc": "2.0", "id": self.seq, "method": method, "params": params}))
        updates = []
        async with asyncio.timeout(120):
            while True:
                message = json.loads(await self.ws.recv())
                self.events.append(message)
                if message.get("method") == "session/request_permission":
                    option = next(o for o in message["params"]["options"] if o["kind"] == "allow_once")
                    await self.ws.send(json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": {
                        "outcome": {"outcome": "selected", "optionId": option["optionId"]}}}))
                elif message.get("id") == self.seq and ("result" in message or "error" in message):
                    assert "error" not in message, message
                    return message["result"], updates
                else:
                    updates.append(message)

    async def prompt(self, sid, text):
        result, updates = await self.call("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text": text}]})
        assert result["stopReason"] == "end_turn", result
        return "".join(u.get("params", {}).get("update", {}).get("content", {}).get("text", "")
            for u in updates if u.get("params", {}).get("update", {}).get("sessionUpdate") == "agent_message_chunk")


async def wait_gone(owned):
    for _ in range(12):
        states = json.loads(execute(
            "import pathlib,json\nresult={}\n"
            f"for pid in {sorted(owned)!r}:\n"
            " try:result[pid]=pathlib.Path('/proc',str(pid),'stat').read_text().rsplit(')',1)[1].split()[0]\n"
            " except FileNotFoundError:pass\nprint(json.dumps(result))"
        ))
        if all(state == "Z" for state in states.values()):
            return len(states)
        await asyncio.sleep(1)
    raise AssertionError("disconnect left connection-owned processes")


async def main():
    status, initial = http("GET", config_path)
    assert status == 200 and initial["status"] == "unconfigured", "use a fresh disposable image"
    isolation_check = b"import os,sys\nif sys.argv[1] != 'session':\n assert 'ANTHROPIC_API_KEY' not in os.environ\n assert not any(k.startswith('KERNEL_CLAUDE_SECRET_') for k in os.environ)\n"
    source = base64.b64encode(isolation_check + Path(__file__).with_name("mcp_checkpoint.py").read_bytes()).decode()
    execute(f"import pathlib,base64; p=pathlib.Path({workspace!r}); p.mkdir(); (p/'mcp.py').write_bytes(base64.b64decode({source!r}))")
    shared = {"name": "checkpoint", "command": python, "args": [workspace + "/mcp.py", "shared", workspace + "/calls"]}
    desired = {"launch": {"model": "claude-haiku-4-5-20251001", "credential": "anthropic"},
        "shared": {"settings": {"language": "English", "alwaysThinkingEnabled": False}, "mcpServers": [shared]}}
    status, ready = http("PUT", config_path, desired, initial["revision"])
    assert status == 200, (status, ready)
    options = {"systemPrompt": "Follow each user instruction exactly. Keep replies short. Remember conversation details. Use MCP tools only when requested.",
        "claudeCode": {"options": {"maxBudgetUsd": 0.15, "maxTurns": 3, "tools": [], "env": {"ENABLE_TOOL_SEARCH": "false"}}}}
    clients = []
    summary = {"pass": False}
    try:
        first = await Client().open()
        clients.append(first)
        session, _ = await first.call("session/new", {"cwd": workspace, "mcpServers": [], "_meta": options})
        sid = session["sessionId"]
        marker = "cobalt-" + uuid.uuid4().hex[:8]
        text = await first.prompt(sid, "Remember the checkpoint " + marker + ". Reply only with the checkpoint. Do not use tools.")
        assert marker in text, text
        print("initial turn passed", flush=True)
        text = await first.prompt(sid, "Call the checkpoint MCP tool once and return its output. Do not use other tools.")
        assert "shared" in text, text
        owned = pids()
        assert owned
        second = await Client().open()
        clients.append(second)
        custom = dict(shared, args=[workspace + "/mcp.py", "session", workspace + "/calls"], env=[])
        other, _ = await second.call("session/new", {"cwd": workspace, "mcpServers": [custom], "_meta": options})
        assert other["sessionId"] != sid and len(pids()) > len(owned)
        text = await second.prompt(other["sessionId"], "Call the checkpoint MCP tool once and return its output. Do not use other tools.")
        assert "session" in text, text
        updated = json.loads(json.dumps(desired))
        updated["shared"]["mcpServers"][0]["args"][1] = "updated-shared"
        active = pids()
        status, latest = await asyncio.to_thread(http, "PUT", config_path, updated, ready["revision"])
        assert status == 200 and active == pids(), "activation changed existing connections"
        assert http("PUT", config_path, desired, ready["revision"])[0] == 409
        broken = json.loads(json.dumps(updated))
        broken["shared"]["mcpServers"][0]["command"] = workspace + "/missing-command"
        status, _ = await asyncio.to_thread(http, "PUT", config_path, broken, latest["revision"])
        assert status == 422
        status, failed = http("GET", config_path)
        assert status == 200 and failed["status"] == "failed"
        assert failed["effectiveRevision"] == latest["revision"] and failed["desired"] == broken
        assert active == pids(), "failed preparation changed existing connections"
        await first.close()
        zombies = await wait_gone(owned)
        text = await second.prompt(other["sessionId"], "Reply only OK. Do not use tools.")
        assert "OK" in text, text
        fresh = await Client().open()
        clients.append(fresh)
        cursor = None
        seen = set()
        listed = []
        while True:
            params = {"cwd": workspace}
            if cursor is not None:
                params["cursor"] = cursor
            page, _ = await fresh.call("session/list", params)
            listed.extend(page["sessions"])
            cursor = page.get("nextCursor")
            if not cursor:
                break
            assert cursor not in seen
            seen.add(cursor)
        assert any(s["sessionId"] == sid for s in listed)
        loaded, history = await fresh.call("session/load", {"sessionId": sid, "cwd": workspace, "mcpServers": [], "_meta": options})
        assert marker in json.dumps(history), "native history not replayed"
        original_model = next(o["currentValue"] for o in session["configOptions"] if o["id"] == "model")
        loaded_model = next(o["currentValue"] for o in loaded["configOptions"] if o["id"] == "model")
        assert loaded_model == original_model == "haiku", loaded_model
        text = await fresh.prompt(sid, "What was the checkpoint in our first exchange? Reply only with it. Do not use tools.")
        assert marker in text, text
        text = await fresh.prompt(sid, "Call the checkpoint MCP tool once and return its output. Do not use other tools.")
        assert "updated-shared" in text, text
        summary = {"pass": True, "sessionId": sid, "initialTurn": True, "sharedMCP": True,
            "sessionMCPOverride": True, "independentProcessTrees": True, "disconnectCleanup": True,
            "activationKeepsConnections": True, "staleWriteRejected": True, "failedPreparationRetainsReady": True,
            "freshListLoadHistoryAndRecall": True, "modelRestored": True,
            "listPages": len(seen) + 1, "updatedMCPOnLoad": True, "unreapedZombiesAfterFirstDisconnect": zombies}
    finally:
        all_owned = pids()
        for client in clients:
            await client.close()
        summary["unreapedZombiesAfterFinalDisconnect"] = await wait_gone(all_owned)
        (evidence / "summary.json").write_text(json.dumps(summary, indent=2))
        (evidence / "events.json").write_text(json.dumps([c.events for c in clients], indent=2))
    print(json.dumps(summary), flush=True)


asyncio.run(main())
print("evidence:", evidence)
