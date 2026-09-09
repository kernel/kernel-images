import { readFileSync } from "node:fs";

const [configPath, name] = process.argv.slice(2);
const config = JSON.parse(readFileSync(configPath, "utf8"));
const server = config.shared.mcpServers.find((server) => server.name === name);
const bindings = JSON.parse(process.env.KERNEL_CLAUDE_BINDINGS);
const env = Object.create(null);
for (const name of [
  "PATH",
  "HOME",
  "USER",
  "LANG",
  "TMPDIR",
  "SSL_CERT_FILE",
  "SSL_CERT_DIR",
]) {
  if (process.env[name] !== undefined) env[name] = process.env[name];
}
for (const [name, binding] of Object.entries(server.envBindings ?? {})) {
  const value = process.env[bindings[binding]];
  if (!value) throw new Error("MCP credential binding is unavailable");
  env[name] = value;
}
process.execve(server.command, [server.command, ...(server.args ?? [])], env);
