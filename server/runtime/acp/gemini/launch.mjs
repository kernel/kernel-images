import { readFileSync } from "node:fs";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const config = JSON.parse(readFileSync(process.argv[2], "utf8"));
const bindings = JSON.parse(process.env.KERNEL_GEMINI_BINDINGS);
const env = Object.fromEntries(
  ["PATH", "HOME", "USER", "LANG", "TMPDIR", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR",
    "GEMINI_CLI_HOME", "GEMINI_CLI_SYSTEM_SETTINGS_PATH",
    "GEMINI_CLI_SYSTEM_DEFAULTS_PATH", "GEMINI_CLI_TRUST_WORKSPACE"]
    .filter((name) => process.env[name] !== undefined)
    .map((name) => [name, process.env[name]]),
);
env.GEMINI_CLI_NO_RELAUNCH = "true";
env.GEMINI_API_KEY = process.env[process.env.KERNEL_GEMINI_PROVIDER_SOURCE];
for (const [alias, source] of Object.entries(bindings)) env[alias] = process.env[source];
// No protocol interception: the CLI owns ACP and its native state.
const child = spawn(process.execPath, [
  fileURLToPath(new URL("./node_modules/@google/gemini-cli/bundle/gemini.js", import.meta.url)),
  "--experimental-acp", "--model", config.launch.model, "--extensions", "none",
], { env, stdio: "inherit" });
for (const signal of ["SIGTERM", "SIGINT"])
  process.on(signal, () => child.kill(signal));
child.on("error", () => process.exit(1));
child.on("exit", (code) => process.exit(code ?? 1));
