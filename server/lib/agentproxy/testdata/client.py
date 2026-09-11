"""Exercise the unmodified acpremote mirror with the official ACP Python client."""
import asyncio
import json
import sys

from acp import connect_to_agent, text_block
from acp.schema import ClientCapabilities, RequestPermissionResponse


class Client:
    def __init__(self):
        self.texts = asyncio.Queue()
        self.permissions = 0

    def on_connect(self, connection):
        pass

    async def session_update(self, session_id, update, **kwargs):
        content = getattr(update, "content", None)
        if getattr(content, "type", None) == "text":
            self.texts.put_nowait(content.text)

    async def request_permission(self, options, session_id, tool_call, **kwargs):
        self.permissions += 1
        return RequestPermissionResponse.model_validate({
            "outcome": {"outcome": "selected", "optionId": options[0].option_id}
        })

    async def ext_notification(self, method, params):
        pass


async def main():
    process = await asyncio.create_subprocess_exec(
        sys.argv[1], "mirror", sys.argv[2],
        stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
    )
    client = Client()
    connection = connect_to_agent(client, process.stdin, process.stdout)
    try:
        initialized = await connection.initialize(protocol_version=1, client_capabilities=ClientCapabilities())
        assert initialized.protocol_version == 1
        first = await connection.new_session(cwd=sys.argv[3], mcp_servers=[])
        second = await connection.new_session(cwd=sys.argv[3], mcp_servers=[])
        assert first.session_id != second.session_id
        result = await connection.prompt(session_id=first.session_id, prompt=[text_block("mirror works")])
        assert result.stop_reason == "end_turn"
        assert await client.texts.get() == "mirror works"
        await connection.prompt(session_id=second.session_id, prompt=[text_block("permission")])
        assert client.permissions == 1
        assert await client.texts.get() == "selected"
        await connection.load_session(session_id=first.session_id, cwd=sys.argv[3], mcp_servers=[])
        assert await client.texts.get() == "mirror works"
        print(json.dumps({"ok": True, "client": "ACP Python SDK via acpremote mirror", "sessions": 2, "permissions": 1}))
    finally:
        await connection.close()
        process.stdin.close()
        try:
            await asyncio.wait_for(process.wait(), 5)
        except TimeoutError:
            process.kill()
            await process.wait()


asyncio.run(asyncio.wait_for(main(), 20))
