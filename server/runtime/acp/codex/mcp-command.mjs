import { run, platformEnvironment } from "./process.mjs";

const [mapping, command, ...args] = process.argv.slice(2);
const env = Object.assign(Object.create(null), platformEnvironment());
for (const [target, source] of JSON.parse(mapping)) {
  if (!process.env[source]) throw new Error("MCP credential unavailable; reconnect to use the active configuration");
  env[target] = process.env[source];
}
run(command, args, env);
