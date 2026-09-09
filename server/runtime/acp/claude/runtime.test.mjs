import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import test from "node:test";

const runtime = dirname(fileURLToPath(import.meta.url));

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "claude-runtime-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const capture = join(root, "capture");
  writeFileSync(
    capture,
    `#!${process.execPath}\nconsole.log(JSON.stringify({args:process.argv.slice(2),env:process.env}));`,
    { mode: 0o700 },
  );
  writeFileSync(
    join(root, "settings.json"),
    JSON.stringify({ language: "English", alwaysThinkingEnabled: false }),
  );
  const config = join(root, "config.json");
  writeFileSync(
    config,
    JSON.stringify({
      shared: {
        mcpServers: [
          {
            name: "docs",
            command: capture,
            args: ["fixture"],
            envBindings: { DOCS_KEY: "docs-token", ["__proto__"]: "docs-token" },
          },
          { name: "other", command: capture, args: [] },
        ],
      },
    }),
  );
  const env = {
    PATH: "/usr/bin:/bin",
    HOME: root,
    KERNEL_CLAUDE_CONFIG: config,
    KERNEL_CLAUDE_NATIVE: capture,
    KERNEL_CLAUDE_BINDINGS: '{"docs-token":"KERNEL_CLAUDE_SECRET_0"}',
    KERNEL_CLAUDE_SECRET_0: "mcp-fixture",
    ANTHROPIC_API_KEY: "provider-fixture",
    UNRELATED_SECRET: "unrelated",
  };
  return { root, capture, config, env };
}

function run(script, args, env) {
  const result = spawnSync(process.execPath, [join(runtime, script), ...args], {
    env,
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  return JSON.parse(result.stdout);
}

test("native flags merge shared MCP underneath ACP names and disable ambient MCP", (t) => {
  const { root, env } = fixture(t);
  const custom = { type: "stdio", command: "/bin/custom", args: [] };
  const { args } = run(
    "native.mjs",
    [
      "--input-format",
      "stream-json",
      "--mcp-config",
      JSON.stringify({ mcpServers: { docs: custom } }),
    ],
    env,
  );
  const mcp = JSON.parse(args[args.indexOf("--mcp-config") + 1]);
  assert.deepEqual(mcp.mcpServers.docs, custom);
  assert.equal(mcp.mcpServers.other.command, process.execPath);
  assert.equal(mcp.mcpServers.other.args[0], join(runtime, "mcp-command.mjs"));
  assert.equal(
    args[args.indexOf("--settings") + 1],
    join(root, "settings.json"),
  );
  assert.equal(args[args.indexOf("--setting-sources") + 1], "");
  assert.ok(args.includes("--strict-mcp-config"));
  assert.ok(!JSON.stringify(args).includes("mcp-fixture"));
  assert.ok(!JSON.stringify(args).includes("provider-fixture"));
});

test("shared settings merge underneath native SDK settings without duplicate flags", (t) => {
  const { root, env } = fixture(t);
  const settings = {
    language: "Spanish",
    env: { ANTHROPIC_BASE_URL: "https://example.test" },
    availableModels: ["haiku"],
  };
  const settingsPath = join(root, "session-settings.json");
  writeFileSync(settingsPath, JSON.stringify(settings));
  for (const supplied of [JSON.stringify(settings), settingsPath]) {
    const { args } = run(
      "native.mjs",
      [
        "--input-format",
        "stream-json",
        "--setting-sources=user,project,local",
        "--settings",
        supplied,
      ],
      env,
    );
    assert.equal(args.filter((arg) => arg === "--settings").length, 1);
    assert.deepEqual(JSON.parse(args[args.indexOf("--settings") + 1]), {
      alwaysThinkingEnabled: false,
      ...settings,
    });
    assert.equal(args[args.lastIndexOf("--setting-sources") + 1], "");
  }
});

test("native version/auth commands retain their original arguments", (t) => {
  const { env } = fixture(t);
  for (const args of [["--version"], ["auth", "status"]]) {
    assert.deepEqual(run("native.mjs", args, env).args, args);
  }
});

test("shared stdio receives only platform environment and explicit bindings", (t) => {
  const { config, env } = fixture(t);
  const result = run("mcp-command.mjs", [config, "docs"], env);
  assert.deepEqual(result.args, ["fixture"]);
  assert.equal(result.env.DOCS_KEY, "mcp-fixture");
  assert.equal(result.env.__proto__, "mcp-fixture");
  for (const name of [
    "ANTHROPIC_API_KEY",
    "UNRELATED_SECRET",
    "KERNEL_CLAUDE_SECRET_0",
    "KERNEL_CLAUDE_BINDINGS",
  ]) {
    assert.equal(result.env[name], undefined);
  }
  delete env.KERNEL_CLAUDE_SECRET_0;
  const failed = spawnSync(
    process.execPath,
    [join(runtime, "mcp-command.mjs"), config, "docs"],
    { env },
  );
  assert.notEqual(failed.status, 0);
});
