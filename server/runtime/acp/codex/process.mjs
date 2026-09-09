import { spawn } from "node:child_process";

export function run(command, args, env) {
  const child = spawn(command, args, { env, stdio: "inherit" });
  for (const signal of ["SIGTERM", "SIGINT"])
    process.on(signal, () => child.kill(signal));
  child.on("error", () => process.exit(1));
  child.on("exit", (code) => process.exit(code ?? 1));
}

export function platformEnvironment() {
  return Object.fromEntries(
    ["PATH", "HOME", "USER", "LANG", "TMPDIR", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR"]
      .filter((name) => process.env[name] !== undefined)
      .map((name) => [name, process.env[name]]),
  );
}
