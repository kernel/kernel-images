import { readFileSync, writeFileSync, renameSync, statSync } from "node:fs";

// pi-acp 0.0.33 stores MCP definitions but does not pass them to Pi. Keep this
// version-specific adapter patch outside the protocol-transparent Go proxy.
const path = new URL("./node_modules/pi-acp/dist/index.js", import.meta.url);
let source = readFileSync(path, "utf8");
const replacements = [
  [
    'cwd: params.cwd,\n      stdio: "pipe",\n      env: process.env,',
    'cwd: params.cwd,\n      stdio: "pipe",\n      env: { ...process.env, KERNEL_PI_SESSION_MCP: JSON.stringify(params.mcpServers ?? []) },',
  ],
  [
    "piCommand: params.piCommand\n",
    "piCommand: params.piCommand,\n        mcpServers: params.mcpServers\n",
  ],
  [
    "piCommand: process.env.PI_ACP_PI_COMMAND\n        });",
    "piCommand: process.env.PI_ACP_PI_COMMAND,\n          mcpServers: opts?.mcpServers ?? []\n        });",
  ],
  [
    "mcpCapabilities: { http: false, sse: false }",
    "mcpCapabilities: { http: true, sse: true }",
  ],
];
for (const [before, after] of replacements) {
  if (source.split(after).length === 2) continue;
  if (source.split(before).length !== 2)
    throw new Error("pinned pi-acp patch no longer matches");
  source = source.replace(before, after);
}
// Replace the inode: Bun can hardlink installed files to its package cache.
const temporary = new URL(path.href + `.${process.pid}.tmp`);
writeFileSync(temporary, source, { mode: statSync(path).mode & 0o777 });
renameSync(temporary, path);
