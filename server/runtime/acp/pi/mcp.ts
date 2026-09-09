import { readFileSync } from "node:fs";
import { join } from "node:path";
import { createMcpAdapter, type ServerEntry } from "pi-mcp-adapter";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

type SharedServer = {
  name: string;
  command?: string;
  args?: string[];
  envBindings?: Record<string, string>;
  url?: string;
  transport?: "http" | "sse";
  headerBindings?: Record<string, { credential: string; prefix?: string }>;
};
type SessionServer = {
  name: string;
  command?: string;
  args?: string[];
  env?: { name: string; value: string }[];
  url?: string;
  type?: "http" | "sse";
  headers?: { name: string; value: string }[];
};

export default function (pi: ExtensionAPI) {
  const preparing = process.env.KERNEL_PI_PREPARING === "1";
  const state = process.env.KERNEL_PI_STATE;
  if (!preparing && !state) throw new Error("managed Pi state is unavailable");
  const config: { shared: { mcpServers: SharedServer[] } } = preparing
    ? { shared: { mcpServers: [] } }
    : JSON.parse(readFileSync(join(state!, "current/config.json"), "utf8"));
  const bindings: Record<string, string> = JSON.parse(
    process.env.KERNEL_PI_BINDINGS ?? "{}",
  );
  const credential = (name: string) => {
    const value = process.env[bindings[name]];
    if (!value) throw new Error("MCP credential binding is unavailable");
    return value;
  };
  const servers: Record<string, ServerEntry> = Object.create(null);
  for (const server of config.shared.mcpServers) {
    servers[server.name] = {
      ...(server.command
        ? {
            command: server.command,
            args: server.args ?? [],
            env: Object.fromEntries(
              Object.entries(server.envBindings ?? {}).map(([key, binding]) => [
                key,
                credential(binding),
              ]),
            ),
          }
        : {
            url: server.url,
            headers: Object.fromEntries(
              Object.entries(server.headerBindings ?? {}).map(
                ([key, binding]) => [
                  key,
                  (binding.prefix ?? "") + credential(binding.credential),
                ],
              ),
            ),
            httpTransport:
              server.transport === "sse" ? "sse" : "streamable-http",
          }),
      lifecycle: "eager",
      directTools: true,
      literalEnv: true,
      inheritEnv: false,
      auth: false,
      oauth: false,
    };
  }
  // Session definitions override shared defaults by name, without writing files.
  const sessionServers: SessionServer[] = preparing
    ? []
    : JSON.parse(process.env.KERNEL_PI_SESSION_MCP ?? "[]");
  for (const server of sessionServers) {
    servers[server.name] = {
      ...(server.command
        ? {
            command: server.command,
            args: server.args ?? [],
            env: Object.fromEntries(
              (server.env ?? []).map(({ name, value }) => [name, value]),
            ),
          }
        : {
            url: server.url,
            headers: Object.fromEntries(
              (server.headers ?? []).map(({ name, value }) => [name, value]),
            ),
            httpTransport: server.type === "sse" ? "sse" : "streamable-http",
          }),
      lifecycle: "eager",
      directTools: true,
      literalEnv: true,
      inheritEnv: false,
      auth: false,
      oauth: false,
    };
  }
  return createMcpAdapter({ config: { mcpServers: servers } })(pi);
}
