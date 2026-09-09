"""Opt-in real-provider gate. Use a fresh disposable image with OPENAI_API_KEY.
Requires the pinned ACP requirements in the test runner's Python environment.
"""

import asyncio, base64, json, os, pathlib, tempfile, urllib.request, urllib.error, uuid
import websockets

os.umask(0o077)
base = os.environ["AGENT_API_URL"].rstrip("/")
assert base.startswith(("http://", "https://")), "AGENT_API_URL must be HTTP(S)"
root = pathlib.Path(tempfile.mkdtemp(prefix="codex-gate-evidence-"))
workspace = "/tmp/codex-gate-" + uuid.uuid4().hex


def http(method, path, data=None, revision=None):
    headers = {"Content-Type": "application/json"}
    if revision:
        headers["If-Match"] = '"' + revision + '"'
    request = urllib.request.Request(
        base + path,
        data=json.dumps(data).encode() if data is not None else None,
        method=method,
        headers=headers,
    )
    try:
        response = urllib.request.urlopen(request, timeout=200)
        return response.status, json.loads(response.read())
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()


def pids():
    code = """import pathlib,json
pids=[]
for path in pathlib.Path('/proc').iterdir():
 if not path.name.isdigit():continue
 try:args=(path/'cmdline').read_bytes().split(bytes([0]))
 except (FileNotFoundError,ProcessLookupError,PermissionError):continue
 if any(a.startswith(b'/opt/kernel-agent/codex/') for a in args):pids.append(path.name)
print(json.dumps(pids))
"""
    status, result = http(
        "POST",
        "/process/exec",
        {"command": "/opt/kernel-agent/venv/bin/python", "args": ["-c", code]},
    )
    assert status == 200 and result["exit_code"] == 0, result
    return set(json.loads(base64.b64decode(result["stdout_b64"])))


config_path = "/agent/v1/harnesses/codex/config"
status, initial = http("GET", config_path)
assert status == 200
assert initial["status"] == "unconfigured", (
    "Use a fresh disposable browser; refusing to replace existing configuration."
)
source = base64.b64encode(
    pathlib.Path(__file__).with_name("mcp_checkpoint.py").read_bytes()
).decode()
code = (
    "import pathlib,base64; p=pathlib.Path('"
    + workspace
    + "'); p.mkdir(); (p/'mcp.py').write_bytes(base64.b64decode('"
    + source
    + "'))"
)
assert (
    http(
        "POST",
        "/process/exec",
        {"command": "/opt/kernel-agent/venv/bin/python", "args": ["-c", code]},
    )[0]
    == 200
)
shared = {
    "name": "checkpoint",
    "command": "/opt/kernel-agent/venv/bin/python",
    "args": [workspace + "/mcp.py", "shared", workspace + "/calls"],
}
desired = {
    "launch": {
        "provider": "openai",
        "model": "gpt-5.4-mini",
        "thinking": "low",
        "credential": "openai",
        "mode": "read-only",
    },
    "shared": {"webSearch": "disabled", "instructions": "Keep replies brief.", "mcpServers": [shared]},
}
status, ready = http("PUT", config_path, desired, initial["revision"])
assert status == 200, (status, ready)
print("configuration ready", ready["revision"], flush=True)


class Client:
    def __init__(self):
        self.seq = 0
        self.events = []

    async def open(self):
        self.ws = await websockets.connect(
            "ws" + base[4:] + "/agent/v1/acp?harness=codex", max_size=1 << 20
        )
        try:
            await self.call(
                "initialize", {"protocolVersion": 1, "clientCapabilities": {}}
            )
            await self.call("authenticate", {"methodId": "api-key"})
        except BaseException:
            await self.ws.close()
            raise
        return self

    async def close(self):
        await self.ws.close()
        await asyncio.sleep(2)

    async def call(self, method, params):
        async with asyncio.timeout(90):
            return await self._call(method, params)

    async def _call(self, method, params):
        self.seq += 1
        await self.ws.send(
            json.dumps(
                {"jsonrpc": "2.0", "id": self.seq, "method": method, "params": params}
            )
        )
        updates = []
        while True:
            message = json.loads(await asyncio.wait_for(self.ws.recv(), 150))
            self.events.append(message)
            if message.get("method") == "session/request_permission":
                option = next(
                    o for o in message["params"]["options"] if o["kind"] == "allow_once"
                )
                await self.ws.send(
                    json.dumps(
                        {
                            "jsonrpc": "2.0",
                            "id": message["id"],
                            "result": {
                                "outcome": {
                                    "outcome": "selected",
                                    "optionId": option["optionId"],
                                }
                            },
                        }
                    )
                )
            elif message.get("id") == self.seq and (
                "result" in message or "error" in message
            ):
                assert "error" not in message, message
                return message["result"], updates
            else:
                updates.append(message)

    async def prompt(self, sid, text):
        _, updates = await self.call(
            "session/prompt",
            {"sessionId": sid, "prompt": [{"type": "text", "text": text}]},
        )
        return "".join(
            u.get("params", {}).get("update", {}).get("content", {}).get("text", "")
            for u in updates
            if u.get("params", {}).get("update", {}).get("sessionUpdate")
            == "agent_message_chunk"
        )


async def main():
    clients = []
    summary = {}
    try:
        first = await Client().open()
        clients.append(first)
        session, _ = await first.call(
            "session/new", {"cwd": workspace, "mcpServers": []}
        )
        sid = session["sessionId"]
        old = pids()
        assert old
        marker = "cobalt-" + uuid.uuid4().hex[:8]
        text = await first.prompt(
            sid,
            "Remember this checkpoint: "
            + marker
            + ". Reply only with the checkpoint. Do not use tools.",
        )
        assert marker in text, text
        second = await Client().open()
        clients.append(second)
        custom = dict(
            shared,
            name="session_checkpoint",
            args=[workspace + "/mcp.py", "session", workspace + "/calls"],
            env=[],
        )
        other, _ = await second.call(
            "session/new", {"cwd": workspace, "mcpServers": [custom]}
        )
        assert other["sessionId"] != sid
        assert len(pids()) > len(old)
        text = await second.prompt(
            other["sessionId"],
            "Call the session_checkpoint MCP checkpoint tool once and return its output. Do not use other tools.",
        )
        assert "session" in text, text
        text = await first.prompt(
            sid,
            "Call the checkpoint MCP tool once and return its output. Do not use other tools.",
        )
        assert "shared" in text, text
        status, calls = http("POST", "/process/exec", {
            "command": "/bin/cat", "args": [workspace + "/calls"]
        })
        assert status == 200 and calls["exit_code"] == 0
        assert set(base64.b64decode(calls["stdout_b64"]).decode().splitlines()) == {"shared", "session"}
        updated = json.loads(json.dumps(desired))
        updated["shared"]["mcpServers"][0]["args"][1] = "updated-shared"
        active = pids()
        code, latest = await asyncio.to_thread(
            http, "PUT", config_path, updated, ready["revision"]
        )
        assert code == 200, (code, latest)
        assert pids() == active, "configuration update restarted active connections"
        code, _ = http("PUT", config_path, updated, ready["revision"])
        assert code == 409, "stale If-Match accepted"
        broken = json.loads(json.dumps(updated))
        broken["shared"]["mcpServers"][0]["command"] = "/nonexistent/codex-gate-mcp"
        code, error = await asyncio.to_thread(
            http, "PUT", config_path, broken, latest["revision"]
        )
        assert code == 422, (code, error)
        code, failed = http("GET", config_path)
        assert (
            failed["status"] == "failed"
            and failed["effectiveRevision"] == latest["revision"]
        )
        # Native forks persist real history without additional provider turns.
        # More than the native 25-item page size forces cursor traversal.
        for _ in range(26):
            forked, _ = await first.call("session/fork", {
                "sessionId": sid, "cwd": workspace, "mcpServers": []
            })
            await first.call("session/close", {"sessionId": forked["sessionId"]})
        await first.close()
        assert not old.intersection(pids()), (old, pids())
        text = await second.prompt(
            other["sessionId"], "Reply only with OK. Do not use tools."
        )
        assert "OK" in text
        reconnected = await Client().open()
        clients.append(reconnected)
        cursor, sessions, pages = None, [], 0
        while True:
            params = {"cwd": workspace}
            if cursor:
                params["cursor"] = cursor
            listed, _ = await reconnected.call("session/list", params)
            pages += 1
            sessions.extend(listed["sessions"])
            cursor = listed.get("nextCursor")
            if not cursor:
                break
            assert pages < 100, "pagination did not terminate"
        assert pages >= 2, "native history did not exercise pagination"
        assert any(s["sessionId"] == sid for s in sessions)
        _, history = await reconnected.call(
            "session/load", {"sessionId": sid, "cwd": workspace, "mcpServers": []}
        )
        assert marker in json.dumps(history)
        text = await reconnected.prompt(
            sid,
            "What was the checkpoint word in our first exchange? Reply only with it. Do not use tools.",
        )
        assert marker in text, text
        text = await reconnected.prompt(
            sid,
            "Call the checkpoint MCP tool once and return its output. Do not use other tools.",
        )
        assert "updated-shared" in text, text
        summary = {
            "configurationUpdateKeepsConnections": True,
            "updatedSharedMCP": True,
            "pass": True,
            "independentConnections": True,
            "failedPreparationRetainsReady": True,
            "disconnectKillsAdapter": True,
            "freshListLoadHistoryAndRecall": True,
            "sharedMCP": True,
            "sessionMCPAddition": True,
            "listPages": pages,
            "sessionId": sid,
        }
    finally:
        for c in clients:
            await c.close()
        assert not pids(), pids()
        (root / "summary.json").write_text(json.dumps(summary, indent=2))
        (root / "events.json").write_text(
            json.dumps([c.events for c in clients], indent=2)
        )
    print(json.dumps(summary), flush=True)


asyncio.run(asyncio.wait_for(main(), 600))
print("evidence:", root)
