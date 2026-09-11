import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { run } from "./process.mjs";

const config = JSON.parse(readFileSync(process.argv[2], "utf8"));
process.env.KERNEL_PI_LAUNCH = JSON.stringify(config.launch);
process.env.KERNEL_PI_NODE = process.execPath;
process.env[process.env.KERNEL_PI_PROVIDER_ENV] =
  process.env[process.env.KERNEL_PI_PROVIDER_SOURCE];
process.env.PI_ACP_PI_COMMAND = fileURLToPath(
  new URL("./pi-command", import.meta.url),
);
process.env.PI_SKIP_VERSION_CHECK = "1";
run(process.execPath, [
  fileURLToPath(
    new URL("./node_modules/pi-acp/dist/index.js", import.meta.url),
  ),
]);
