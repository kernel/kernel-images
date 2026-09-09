"""Opt-in Gemini 0.58.0 packaged-image gate: fresh sessions only, no restoration.
Run with AGENT_API_URL and the pinned runtime/acp/requirements.txt Python environment.
The disposable image must have GEMINI_API_KEY and GEMINI_GATE_MCP_TOKEN=fixture-token.
At most four bounded prompts; no prompt retries. No raw ACP output is persisted.
"""

import asyncio
import base64
import json
import os
import pathlib
import urllib.error
import urllib.request
import uuid

import websockets

base = os.environ["AGENT_API_URL"].rstrip("/")
workspace = "/tmp/gemini-gate-" + uuid.uuid4().hex
config_path = "/agent/v1/harnesses/gemini/config"
python = "/opt/kernel-agent/venv/bin/python"


def http(method, path, data=None, revision=None):
    headers = {"Content-Type": "application/json"}
    if revision is not None:
        headers["If-Match"] = '"' + revision + '"'
    request = urllib.request.Request(
        base + path, data=json.dumps(data).encode() if data is not None else None,
        method=method, headers=headers,
    )
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()


def remote(code):
    status, response = http("POST", "/process/exec", {"command": python, "args": ["-c", code]})
    assert status == 200 and response["exit_code"] == 0, "remote fixture command failed"
    return base64.b64decode(response["stdout_b64"]).decode()


def pids():
    return set(json.loads(remote("""import pathlib,json
pids=[]
for path in pathlib.Path('/proc').iterdir():
 if not path.name.isdigit():continue
 try:args=(path/'cmdline').read_bytes().split(bytes([0]))
 except (FileNotFoundError,ProcessLookupError,PermissionError):continue
 if b'/opt/kernel-agent/gemini/node_modules/@google/gemini-cli/bundle/gemini.js' in args:pids.append(path.name)
print(json.dumps(pids))
""")))


class Client:
    def __init__(self):
        self.seq = 0
        self.permissions = 0

    async def open(self):
        self.ws = await websockets.connect("ws" + base[4:] + "/agent/v1/acp?harness=gemini", max_size=1 << 20)
        try:
            result, _ = await self.call("initialize", {"protocolVersion": 1, "clientCapabilities": {}})
            assert result["agentInfo"]["version"] == "0.58.0"
            assert any(m["id"] == "gemini-api-key" for m in result["authMethods"])
            await self.call("authenticate", {"methodId": "gemini-api-key"})
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
        async with asyncio.timeout(90):
            while True:
                message = json.loads(await self.ws.recv())
                if message.get("method") == "session/request_permission":
                    self.permissions += 1
                    option = next(o for o in message["params"]["options"] if o["kind"] == "allow_once")
                    await self.ws.send(json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": {
                        "outcome": {"outcome": "selected", "optionId": option["optionId"]}
                    }}))
                elif message.get("id") == self.seq and ("result" in message or "error" in message):
                    assert "error" not in message, f"native {method} failed (error output intentionally omitted)"
                    return message["result"], updates
                else:
                    updates.append(message)

    async def prompt(self, sid, text):
        _, updates = await self.call("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text": text}]})
        return "".join(u.get("params", {}).get("update", {}).get("content", {}).get("text", "")
            for u in updates if u.get("params", {}).get("update", {}).get("sessionUpdate") == "agent_message_chunk")


async def main():
    status, initial = http("GET", config_path)
    assert status == 200 and initial["status"] == "unconfigured", "use a fresh disposable image"
    assert not pids(), "refusing a browser with existing Gemini processes"
    # Record only boolean isolation evidence, never an environment dump.
    fixture = pathlib.Path(__file__).with_name("mcp_checkpoint.py").read_text()
    fixture = "import os\nassert not os.getenv('GEMINI_API_KEY')\nassert not any(k.startswith('KERNEL_GEMINI_SECRET_') for k in os.environ)\nassert os.getenv('DOCS_TOKEN') == 'fixture-token'\n" + fixture
    encoded = base64.b64encode(fixture.encode()).decode()
    remote(f"import pathlib,base64; p=pathlib.Path('{workspace}');p.mkdir();(p/'mcp.py').write_bytes(base64.b64decode('{encoded}'))")
    shared = {"name": "checkpoint", "command": python,
        "args": [workspace + "/mcp.py", "shared-marker", workspace + "/calls"],
        "envBindings": {"DOCS_TOKEN": "gate-mcp"}}
    desired = {"launch": {"model": "gemini-2.5-flash", "credential": "google", "trustWorkspace": True},
        "shared": {"settings": {"maxSessionTurns": 6}, "mcpServers": [shared]}}
    assert http("PUT", config_path, desired)[0] == 428
    status, ready = http("PUT", config_path, desired, initial["revision"])
    assert status == 200, "initial preparation failed"
    assert http("PUT", config_path, desired, initial["revision"])[0] == 409
    invalid = json.loads(json.dumps(desired))
    invalid["shared"]["settings"]["maxSessionTurns"] = -1
    assert http("PUT", config_path, invalid, ready["revision"])[0] == 422
    clients = []
    try:
        first = await Client().open()
        clients.append(first)
        session, _ = await first.call("session/new", {"cwd": workspace, "mcpServers": []})
        old = pids()
        assert len(old) == 1
        second = await Client().open()
        clients.append(second)
        override = {"name": "checkpoint", "command": python,
            "args": [workspace + "/mcp.py", "session-marker", workspace + "/calls"],
            "env": [{"name": "DOCS_TOKEN", "value": "fixture-token"}]}
        other, _ = await second.call("session/new", {"cwd": workspace, "mcpServers": [override]})
        assert other["sessionId"] != session["sessionId"] and len(pids()) == 2
        for client, sid, marker in [(first, session["sessionId"], "shared-marker"), (second, other["sessionId"], "session-marker")]:
            text = await client.prompt(sid, "Call the checkpoint MCP tool exactly once and return its output. Do not use any other tools.")
            assert marker in text, "MCP marker missing from native response"
        assert first.permissions + second.permissions > 0, "no native permission requests observed"
        calls = remote(f"from pathlib import Path;print(Path('{workspace}/calls').read_text())")
        assert "shared-marker" in calls and "session-marker" in calls
        active = pids()
        updated = json.loads(json.dumps(desired))
        updated["shared"]["settings"]["maxSessionTurns"] = 7
        status, latest = await asyncio.to_thread(http, "PUT", config_path, updated, ready["revision"])
        assert status == 200 and pids() == active
        # Break only the packaged executable temporarily to test preparation failure retention.
        bundle = "/opt/kernel-agent/gemini/node_modules/@google/gemini-cli/bundle/gemini.js"
        remote(f"from pathlib import Path;p=Path('{bundle}');p.rename(str(p)+'.gate-backup')")
        try:
            assert (await asyncio.to_thread(http, "PUT", config_path, updated, latest["revision"]))[0] == 422
        finally:
            remote(f"from pathlib import Path;Path('{bundle}.gate-backup').rename('{bundle}')")
        _, failed = http("GET", config_path)
        assert failed["status"] == "failed" and failed["effectiveRevision"] == latest["revision"]
        assert pids() == active
        await first.close()
        for _ in range(20):
            if not old.intersection(pids()):
                break
            await asyncio.sleep(0.5)
        assert not old.intersection(pids()), "disconnect left native process alive"
        assert len(pids()) == 1, "disconnect killed another connection"
        text = await second.prompt(other["sessionId"], "Reply only with OK. Do not use tools.")
        assert "OK" in text, "existing connection failed after revision update"
        # Fresh-session operation after failure uses the retained ready revision.
        third = await Client().open()
        clients.append(third)
        fresh, _ = await third.call("session/new", {"cwd": workspace, "mcpServers": []})
        assert fresh["sessionId"] not in (session["sessionId"], other["sessionId"])
        text = await third.prompt(fresh["sessionId"], "Reply only with READY. Do not use tools.")
        assert "READY" in text
        print(json.dumps({"pass": True, "version": "0.58.0", "model": "gemini-2.5-flash", "prompts": 4,
            "authentication": True, "sharedMCP": True, "sessionMCPOverride": True, "credentialIsolation": True,
            "nativePermissions": True, "independentConnections": True, "retainedEffectiveRevision": True,
            "newSessionsOnly": True, "reconnectDiscoveryLoadHistory": "unsupported; not exercised"}), flush=True)
    finally:
        for client in clients:
            await client.close()
        for _ in range(20):
            if not pids():
                break
            await asyncio.sleep(0.5)
        assert not pids(), "native processes remained after disconnect"


asyncio.run(main())
