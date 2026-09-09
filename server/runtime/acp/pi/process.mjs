import { spawn } from "node:child_process";

export function run(command, args, env = process.env) {
  const child = spawn(command, args, { env, stdio: "inherit" });
  for (const signal of ["SIGTERM", "SIGINT"])
    process.on(signal, () => child.kill(signal));
  child.on("error", () => process.exit(1));
  child.on("exit", (code) => process.exit(code ?? 1));
}
