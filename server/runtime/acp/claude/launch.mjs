import { readFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { claudeCliPath } from "@agentclientprotocol/claude-agent-acp/dist/acp-agent.js";

const configPath = process.argv[2];
const config = JSON.parse(readFileSync(configPath, "utf8"));
const bindings = JSON.parse(process.env.KERNEL_CLAUDE_BINDINGS);
const env = Object.create(null);
for (const name of [
  "PATH",
  "HOME",
  "USER",
  "LANG",
  "TMPDIR",
  "TERM",
  "SSL_CERT_FILE",
  "SSL_CERT_DIR",
  "CLAUDE_CONFIG_DIR",
]) {
  if (process.env[name] !== undefined) env[name] = process.env[name];
}
const aliases = Object.create(null);
for (const [binding, source] of Object.entries(bindings)) {
  const alias = `KERNEL_CLAUDE_SECRET_${Object.keys(aliases).length}`;
  if (!process.env[source])
    throw new Error("credential binding is unavailable");
  aliases[binding] = alias;
  env[alias] = process.env[source];
}
env.ANTHROPIC_API_KEY = env[aliases[config.launch.credential]];
env.ANTHROPIC_MODEL = config.launch.model;
env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1";
env.KERNEL_CLAUDE_BINDINGS = JSON.stringify(aliases);
env.KERNEL_CLAUDE_CONFIG = configPath;
env.KERNEL_CLAUDE_NATIVE = await claudeCliPath();
env.KERNEL_CLAUDE_NODE = process.execPath;
const runtime = dirname(fileURLToPath(import.meta.url));
env.CLAUDE_CODE_EXECUTABLE = join(runtime, "native-command");
mkdirSync(env.HOME, { recursive: true, mode: 0o700 });
mkdirSync(env.CLAUDE_CONFIG_DIR, { recursive: true, mode: 0o700 });
const adapter = join(
  runtime,
  "node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js",
);
process.execve(process.execPath, [process.execPath, adapter], env);
