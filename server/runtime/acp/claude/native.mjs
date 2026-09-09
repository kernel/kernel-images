import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const args = process.argv.slice(2);
// Only augment SDK query launches, not native auth, version or status commands.
if (args.includes("--input-format")) {
  const configPath = process.env.KERNEL_CLAUDE_CONFIG;
  const config = JSON.parse(readFileSync(configPath, "utf8"));
  const runtime = dirname(fileURLToPath(import.meta.url));
  const servers = Object.create(null);
  for (const server of config.shared.mcpServers) {
    servers[server.name] = {
      type: "stdio",
      command: process.execPath,
      args: [join(runtime, "mcp-command.mjs"), configPath, server.name],
    };
  }
  // The SDK sends its ACP servers as one JSON --mcp-config argument. Merge
  // defaults underneath them; leave session names and conversation data alone.
  const index = args.indexOf("--mcp-config");
  if (index !== -1) {
    const supplied = JSON.parse(args[index + 1]);
    args[index + 1] = JSON.stringify({
      ...supplied,
      mcpServers: { ...servers, ...supplied.mcpServers },
    });
  } else if (Object.keys(servers).length) {
    args.push("--mcp-config", JSON.stringify({ mcpServers: servers }));
  }
  const settingsPath = join(dirname(configPath), "settings.json");
  const settingsIndex = args.indexOf("--settings");
  if (settingsIndex !== -1) {
    const value = args[settingsIndex + 1];
    const supplied = JSON.parse(
      value.trimStart().startsWith("{") ? value : readFileSync(value, "utf8"),
    );
    const shared = JSON.parse(readFileSync(settingsPath, "utf8"));
    args[settingsIndex + 1] = JSON.stringify({ ...shared, ...supplied });
  } else {
    args.push("--settings", settingsPath);
  }
  args.push("--setting-sources", "", "--strict-mcp-config");
}
process.execve(
  process.env.KERNEL_CLAUDE_NATIVE,
  [process.env.KERNEL_CLAUDE_NATIVE, ...args],
  process.env,
);
