import { mkdir, readFile, writeFile, symlink } from "node:fs/promises";
import { spawn } from "node:child_process";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const [revision, state] = process.argv.slice(2);
const config = JSON.parse(
  await readFile(join(revision, "config.json"), "utf8"),
);
const runtime = fileURLToPath(new URL(".", import.meta.url));
await mkdir(join(state, "home"), { recursive: true, mode: 0o700 });
await mkdir(join(state, "sessions"), { recursive: true, mode: 0o700 });
await mkdir(join(revision, "agent"), { mode: 0o700 });
await symlink(join(state, "sessions"), join(revision, "agent/sessions"));
const dependencies = Object.create(null);
for (const source of config.shared.extensions) {
  const spec = source.slice(4),
    index = spec.lastIndexOf("@");
  const name = spec.slice(0, index);
  if (name in dependencies) throw new Error("duplicate extension package");
  dependencies[name] = spec.slice(index + 1);
}
await writeFile(
  join(revision, "package.json"),
  JSON.stringify({ private: true, dependencies }),
  { mode: 0o600 },
);
if (Object.keys(dependencies).length) {
  await new Promise((resolve, reject) => {
    const child = spawn(
      process.env.KERNEL_PI_BUN,
      [
        "install",
        "--ignore-scripts",
        "--no-progress",
        "--registry",
        process.env.KERNEL_PI_REGISTRY,
      ],
      { cwd: revision, stdio: "ignore" },
    );
    child.on("error", reject);
    child.on("exit", (code) =>
      code === 0
        ? resolve()
        : reject(new Error("extension installation failed")),
    );
  });
}
const settings = {
  quietStartup: true,
  enableInstallTelemetry: false,
  enableAnalytics: false,
  defaultProjectTrust: "never",
  extensions: [join(runtime, "mcp.ts")],
  packages: Object.keys(dependencies).map((name) => ({
    source: join(revision, "node_modules", name),
    skills: [],
    prompts: [],
    themes: [],
  })),
};
await writeFile(
  join(revision, "agent/settings.json"),
  JSON.stringify(settings),
  { mode: 0o600 },
);
// Import and initialize installed extensions without provider calls or MCP startup.
// Pi reports broken extensions on stderr but can otherwise continue successfully.
await new Promise((resolve, reject) => {
  const probe = spawn(
    process.execPath,
    [
      join(runtime, "node_modules/@earendil-works/pi-coding-agent/dist/cli.js"),
      "--mode",
      "rpc",
      "--session-dir",
      join(revision, "validation-sessions"),
      "--no-approve",
      "--no-skills",
      "--no-prompt-templates",
      "--no-context-files",
    ],
    {
      cwd: revision,
      env: {
        ...process.env,
        PI_CODING_AGENT_DIR: join(revision, "agent"),
        PI_OFFLINE: "1",
        KERNEL_PI_PREPARING: "1",
      },
      stdio: ["pipe", "pipe", "pipe"],
    },
  );
  let output = "",
    stderr = "",
    complete = false;
  const timer = setTimeout(() => {
    probe.kill("SIGKILL");
    reject(new Error("extension validation timed out"));
  }, 20000);
  probe.on("error", (error) => {
    clearTimeout(timer);
    reject(error);
  });
  probe.stderr.on("data", (data) => {
    stderr += data;
    if (stderr.length > 65536) probe.kill("SIGKILL");
  });
  probe.stdout.on("data", (data) => {
    output += data;
    if (output.length > 65536) probe.kill("SIGKILL");
    let index;
    while ((index = output.indexOf("\n")) >= 0) {
      const line = output.slice(0, index);
      output = output.slice(index + 1);
      try {
        const response = JSON.parse(line);
        if (response.id === "prepare" && response.success === true) {
          complete = true;
          probe.stdin.end();
        }
      } catch {
        /* The Pi CLI can print a startup prelude before RPC output. */
      }
    }
  });
  probe.on("exit", () => {
    clearTimeout(timer);
    if (complete && !stderr.trim()) resolve();
    else reject(new Error("extension validation failed"));
  });
  probe.stdin.on("error", () => {});
  probe.stdin.write(
    JSON.stringify({ id: "prepare", type: "get_state" }) + "\n",
  );
});
