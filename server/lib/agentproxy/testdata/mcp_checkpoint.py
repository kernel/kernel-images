import sys, json

marker = sys.argv[1]
for line in sys.stdin:
    try:
        r = json.loads(line)
    except ValueError:
        continue
    if "id" not in r:
        continue
    method = r["method"]
    if method == "initialize":
        result = {
            "protocolVersion": r["params"]["protocolVersion"],
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "checkpoint", "version": "1"},
        }
    elif method == "tools/list":
        result = {
            "tools": [
                {
                    "name": "checkpoint",
                    "description": "Return the checkpoint marker.",
                    "inputSchema": {"type": "object", "properties": {}},
                }
            ]
        }
    elif method == "tools/call":
        open(sys.argv[2], "a").write(marker + "\n")
        result = {"content": [{"type": "text", "text": marker}]}
    elif method == "ping":
        result = {}
    else:
        print(
            json.dumps(
                {
                    "jsonrpc": "2.0",
                    "id": r["id"],
                    "error": {"code": -32601, "message": "unknown method"},
                }
            ),
            flush=True,
        )
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": r["id"], "result": result}), flush=True)
