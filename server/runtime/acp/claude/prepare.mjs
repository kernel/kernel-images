import { access, readFile, stat, writeFile } from "node:fs/promises";
import { constants } from "node:fs";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import { resolveSettings } from "@anthropic-ai/claude-agent-sdk";
import { claudeCliPath } from "@agentclientprotocol/claude-agent-acp/dist/acp-agent.js";

const [revision] = process.argv.slice(2);
const config = JSON.parse(
  await readFile(join(revision, "config.json"), "utf8"),
);
await writeFile(
  join(revision, "settings.json"),
  JSON.stringify(config.shared.settings),
  { mode: 0o600 },
);
const { effective } = await resolveSettings({
  cwd: revision,
  settingSources: ["user"],
});
for (const [name, value] of Object.entries(config.shared.settings)) {
  if (effective[name] !== value)
    throw new Error("native settings validation failed");
}
for (const server of config.shared.mcpServers) {
  await access(server.command, constants.X_OK);
  if (!(await stat(server.command)).isFile())
    throw new Error("MCP command must be an executable file");
}
execFileSync(await claudeCliPath(), ["--version"], {
  stdio: "ignore",
  timeout: 20000,
});
