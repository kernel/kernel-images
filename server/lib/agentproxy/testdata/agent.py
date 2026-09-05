"""Deterministic ACP stdio peer; no model calls or provider credentials."""
import json
import os
from pathlib import Path
import sys
import uuid

root = Path(os.environ["AGENT_PROXY_TEST_DIR"])
pending = None


def send(value):
    print(json.dumps(value), flush=True)


def reply(request_id, result):
    send({"jsonrpc": "2.0", "id": request_id, "result": result})


def update(session_id, content):
    send({"jsonrpc": "2.0", "method": "session/update", "params": {
        "sessionId": session_id,
        "update": {"sessionUpdate": "agent_message_chunk", "content": content},
    }})


for line in sys.stdin:
    if not line.strip():
        continue
    message = json.loads(line)
    method = message.get("method")
    params = message.get("params", {})
    request_id = message.get("id")
    if method == "initialize":
        reply(request_id, {"protocolVersion": 1, "agentInfo": {"name": "test-peer", "version": "1"},
                           "agentCapabilities": {"loadSession": True,
                               "promptCapabilities": {"image": True, "audio": True, "embeddedContext": True},
                               "sessionCapabilities": {"list": {}, "resume": {}}},
                           "authMethods": [], "_meta": {"pid": os.getpid()}})
    elif method == "session/new":
        session_id = str(uuid.uuid4())
        (root / (session_id + ".json")).write_text(json.dumps([]))
        reply(request_id, {"sessionId": session_id})
    elif method in ("session/load", "session/resume"):
        session_id = params["sessionId"]
        if str(uuid.UUID(session_id)) != session_id:
            raise ValueError("invalid fixture session ID")
        history = json.loads((root / (session_id + ".json")).read_text())
        if method == "session/load":
            for content in history:
                update(session_id, content)
        reply(request_id, {})
    elif method == "session/list":
        reply(request_id, {"sessions": [{"sessionId": path.stem, "cwd": str(root)} for path in root.glob("*.json")]})
    elif method == "session/prompt":
        session_id = params["sessionId"]
        content = params["prompt"]
        text = content[0].get("text", "")
        if text == "permission":
            pending = (request_id, session_id)
            send({"jsonrpc": "2.0", "id": "approval", "method": "session/request_permission", "params": {
                "sessionId": session_id, "toolCall": {"toolCallId": "tool-1", "title": "checkpoint", "status": "pending"},
                "options": [{"optionId": "allow", "kind": "allow_once", "name": "Allow"}],
            }})
        elif text == "block":
            pending = (request_id, session_id)
            update(session_id, {"type": "text", "text": "started"})
        else:
            (root / (session_id + ".json")).write_text(json.dumps(content))
            for block in content:
                update(session_id, block)
            reply(request_id, {"stopReason": "end_turn", "_meta": {"preserved": True}})
    elif method == "session/cancel" and pending:
        reply(pending[0], {"stopReason": "cancelled"})
        pending = None
    elif method is None and request_id == "approval" and pending:
        update(pending[1], {"type": "text", "text": message["result"]["outcome"]["outcome"]})
        reply(pending[0], {"stopReason": "end_turn"})
        pending = None
    elif method and request_id is not None:
        send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "not implemented"}})
