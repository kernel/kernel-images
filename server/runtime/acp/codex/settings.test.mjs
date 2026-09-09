import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtemp, mkdir, writeFile, copyFile, readlink, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import { parse, stringify } from "smol-toml";
import { aliases, settings } from "./settings.mjs";

const config = {
  launch: { provider: "openai", model: "gpt-5.4-mini", thinking: "low", mode: "read-only", credential: "provider" },
  shared: {
    instructions: 'Use "native" settings.\nNo invented escaping.\t',
    webSearch: "disabled",
    mcpServers: [
      { name: "stdio", command: "/bin/echo", args: ["hello"], envBindings: { TOKEN: "mcp" } },
      { name: "http", url: "https://example.com/mcp", transport: "http", headerBindings: { Authorization: { credential: "mcp", prefix: "Bearer " } } },
    ],
  },
};

test("native TOML round trips and contains references, never secret values", () => {
  const native = parse(stringify(settings(config)));
  assert.equal(native.model, config.launch.model);
  assert.equal(native.developer_instructions, config.shared.instructions);
  assert.equal(native.cli_auth_credentials_store, "ephemeral");
  assert.deepEqual(native.features, { shell_snapshot: false, shell_snapshot_v2: false, plugins: false });
  assert.deepEqual(native.mcp_servers.stdio.env_vars, aliases(config.shared.mcpServers[0]).map(([, alias]) => alias));
  assert.deepEqual(native.mcp_servers.http.env_http_headers, Object.fromEntries(aliases(config.shared.mcpServers[1])));
});

test("changed server destinations or bindings cannot reuse existing credential aliases", () => {
  const server = config.shared.mcpServers[1];
  const before = aliases(server);
  assert.notDeepEqual(aliases({ ...server, url: "https://other.example/mcp" }), before);
  assert.notDeepEqual(aliases({ ...server, headerBindings: { Authorization: { credential: "different" } } }), before);
});

test("MCP stdio bindings remap without inheriting provider or unrelated credentials", () => {
  const source = 'process.stdout.write(JSON.stringify(process.env))';
  const output = execFileSync(process.execPath, [new URL("./mcp-command.mjs", import.meta.url).pathname,
    JSON.stringify([["TOKEN", "KERNEL_CODEX_MCP_TEST"], ["__proto__", "KERNEL_CODEX_MCP_TEST"]]), process.execPath, "-e", source], {
    env: { PATH: process.env.PATH, KERNEL_CODEX_MCP_TEST: "mcp-fixture", CODEX_API_KEY: "provider-fixture", UNRELATED_SECRET: "unrelated-fixture" },
  });
  const env = JSON.parse(output);
  assert.equal(env.TOKEN, "mcp-fixture");
  assert.equal(env.__proto__, "mcp-fixture");
  for (const name of ["CODEX_API_KEY", "UNRELATED_SECRET", "KERNEL_CODEX_MCP_TEST"]) assert.equal(env[name], undefined);
});

test("launch preserves native home, sanitizes bindings and rejects replaced native settings", async () => {
  const root = await mkdtemp(join(tmpdir(), "codex-launch-test-"));
  try {
    const runtime = join(root, "runtime"), state = join(root, "state");
    const adapterDir = join(runtime, "node_modules/@agentclientprotocol/codex-acp/dist");
    await mkdir(adapterDir, { recursive: true });
    for (const file of ["launch.mjs", "settings.mjs", "process.mjs"]) await copyFile(new URL(file, import.meta.url), join(runtime, file));
    await writeFile(join(adapterDir, "index.js"), 'process.stdout.write(JSON.stringify(process.env))');
    await mkdir(join(state, "current"), { recursive: true });
    const revision = join(state, "current/config.json");
    await writeFile(revision, JSON.stringify(config));
    const home = join(state, "native");
    const options = { env: {
      PATH: process.env.PATH, HOME: join(state, "home"), CODEX_HOME: home,
      KERNEL_CODEX_STATE: state, KERNEL_CODEX_BINDINGS: JSON.stringify({ provider: "KEY_SOURCE", mcp: "MCP_SOURCE" }),
      KEY_SOURCE: "provider-fixture", MCP_SOURCE: "mcp-fixture", UNRELATED_SECRET: "unrelated-fixture",
      APP_SERVER_LOGS: join(root, "must-not-log"), DEFAULT_AUTH_REQUEST: "must-not-inherit",
    } };
    const run = () => JSON.parse(execFileSync(process.execPath, [join(runtime, "launch.mjs"), revision], options));
    const env = run();
    assert.equal(env.CODEX_API_KEY, "provider-fixture");
    assert.equal(env.CODEX_HOME, home);
    assert.deepEqual(JSON.parse(env.CODEX_CONFIG).features, { shell_snapshot: false, shell_snapshot_v2: false, plugins: false });
    for (const name of ["KEY_SOURCE", "MCP_SOURCE", "KERNEL_CODEX_BINDINGS", "UNRELATED_SECRET", "APP_SERVER_LOGS", "DEFAULT_AUTH_REQUEST"]) assert.equal(env[name], undefined);
    assert.equal(env[aliases(config.shared.mcpServers[1])[0][1]], "Bearer mcp-fixture");
    assert.equal(await readlink(join(home, "config.toml")), join(state, "current/config.toml"));
    await writeFile(join(home, "history-fixture"), "retain me");
    assert.equal(run().CODEX_HOME, home);
    await rm(join(home, "config.toml"));
    await writeFile(join(home, "config.toml"), "unmanaged");
    assert.throws(() => execFileSync(process.execPath, [join(runtime, "launch.mjs"), revision], { ...options, stdio: "ignore" }));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
