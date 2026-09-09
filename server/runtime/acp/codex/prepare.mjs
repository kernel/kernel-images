import { access, mkdir, readFile, writeFile } from "node:fs/promises";
import { constants } from "node:fs";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import { stringify } from "smol-toml";
import { adapter, codex, settings } from "./settings.mjs";

process.umask(0o077);
const [revision] = process.argv.slice(2);
const config = JSON.parse(await readFile(join(revision, "config.json"), "utf8"));
for (const server of config.shared.mcpServers) {
  if (server.command) await access(server.command, constants.X_OK);
}
const native = stringify(settings(config));
await writeFile(join(revision, "config.toml"), native, { mode: 0o600 });
// Validate using an isolated native home, without contacting providers or MCPs.
const probe = join(revision, "validation-home");
await mkdir(probe, { mode: 0o700 });
await writeFile(join(probe, "config.toml"), native, { mode: 0o600 });
const options = {
  cwd: revision,
  env: { PATH: process.env.PATH, HOME: revision, CODEX_HOME: probe },
  timeout: 15000,
  stdio: ["ignore", "pipe", "ignore"],
  maxBuffer: 1 << 20,
};
if (execFileSync(process.execPath, [adapter, "--version"], options).toString().trim() !== "@agentclientprotocol/codex-acp 1.10.0")
  throw new Error("unexpected adapter version");
if (execFileSync(process.execPath, [codex, "--version"], options).toString().trim() !== "codex-cli 0.153.3")
  throw new Error("unexpected Codex version");
execFileSync(process.execPath, [codex, "mcp", "list", "--json"], options);
