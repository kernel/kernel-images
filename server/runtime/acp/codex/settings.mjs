import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";

export const adapter = fileURLToPath(new URL("./node_modules/@agentclientprotocol/codex-acp/dist/index.js", import.meta.url));
export const codex = fileURLToPath(new URL("./node_modules/@openai/codex/bin/codex.js", import.meta.url));

// Bind aliases to the complete server definition: an old connection must not
// send its previous credential to a changed URL after shared settings activate.
export function aliases(server) {
  const id = createHash("sha256").update(JSON.stringify(server)).digest("hex");
  const prefix = `KERNEL_CODEX_MCP_${id}_`;
  return Object.keys(server.command ? server.envBindings ?? {} : server.headerBindings ?? {})
    .sort().map((name, index) => [name, prefix + index]);
}

// Disable credential-bearing shell snapshots and unpinned plugin catalog sync.
// Also enforce these settings as session overrides against project config.
export const safetySettings = {
  cli_auth_credentials_store: "ephemeral",
  features: { shell_snapshot: false, shell_snapshot_v2: false, plugins: false },
};

export function settings(config) {
  return {
    model: config.launch.model,
    model_provider: "openai",
    model_reasoning_effort: config.launch.thinking,
    ...safetySettings,
    forced_login_method: "api",
    web_search: config.shared.webSearch,
    developer_instructions: config.shared.instructions ?? "",
    analytics: { enabled: false },
    mcp_servers: Object.fromEntries(config.shared.mcpServers.map((server) => {
      const names = aliases(server);
      if (server.command) return [server.name, {
        command: process.execPath,
        args: [fileURLToPath(new URL("./mcp-command.mjs", import.meta.url)), JSON.stringify(names), server.command, ...(server.args ?? [])],
        env_vars: names.map(([, name]) => name),
      }];
      return [server.name, {
        url: server.url,
        env_http_headers: Object.fromEntries(names),
      }];
    })),
  };
}
