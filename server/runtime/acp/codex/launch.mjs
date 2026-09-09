import { mkdir, readFile, readlink, symlink } from "node:fs/promises";
import { join } from "node:path";
import { adapter, aliases, safetySettings } from "./settings.mjs";
import { run, platformEnvironment } from "./process.mjs";

process.umask(0o077);
const config = JSON.parse(await readFile(process.argv[2], "utf8"));
const bindings = JSON.parse(process.env.KERNEL_CODEX_BINDINGS);
const credential = (name) => {
  const value = process.env[bindings[name]];
  if (!value) throw new Error("credential binding unavailable");
  return value;
};
const home = process.env.CODEX_HOME;
await mkdir(process.env.HOME, { recursive: true, mode: 0o700 });
await mkdir(home, { recursive: true, mode: 0o700 });
const target = join(process.env.KERNEL_CODEX_STATE, "current", "config.toml");
try {
  await symlink(target, join(home, "config.toml"));
} catch (error) {
  if (error.code !== "EEXIST" || await readlink(join(home, "config.toml")) !== target) throw error;
}
const env = {
  ...platformEnvironment(),
  CODEX_HOME: home,
  CODEX_CONFIG: JSON.stringify(safetySettings),
  CODEX_API_KEY: credential(config.launch.credential),
  MODEL_PROVIDER: "openai",
  INITIAL_AGENT_MODE: config.launch.mode,
  NO_BROWSER: "1",
};
for (const server of config.shared.mcpServers) {
  for (const [name, alias] of aliases(server)) {
    if (server.command) env[alias] = credential(server.envBindings[name]);
    else {
      const header = server.headerBindings[name];
      const value = (header.prefix ?? "") + credential(header.credential);
      if (/[\r\n\0]/.test(value)) throw new Error("invalid MCP header credential");
      env[alias] = value;
    }
  }
}
run(process.execPath, [adapter], env);
