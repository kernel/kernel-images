import { fileURLToPath } from "node:url";
import { run } from "./process.mjs";

const { provider, model, thinking } = JSON.parse(process.env.KERNEL_PI_LAUNCH);
run(process.execPath, [
  fileURLToPath(
    new URL(
      "./node_modules/@earendil-works/pi-coding-agent/dist/cli.js",
      import.meta.url,
    ),
  ),
  "--provider",
  provider,
  "--model",
  model,
  "--thinking",
  thinking,
  "--no-approve",
  ...process.argv.slice(2),
]);
