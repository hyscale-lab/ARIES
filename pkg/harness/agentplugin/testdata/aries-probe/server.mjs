// Minimal stdio MCP server for ARIES's portable plugin tests. One tool,
// aries_plugin_probe, answers with the marker from its environment, its
// working directory, and how many earlier calls left a file in PLUGIN_DATA:
// zero in a fresh occurrence shows the data directory is not shared.
import { readdirSync, writeFileSync } from "node:fs";
import { createInterface } from "node:readline";

const marker = process.env.ARIES_PROBE_MARKER ?? "<no marker>";
const data = process.env.PLUGIN_DATA ?? "";
const send = (message) => process.stdout.write(JSON.stringify({ jsonrpc: "2.0", ...message }) + "\n");

createInterface({ input: process.stdin }).on("line", (line) => {
  let request;
  try {
    request = JSON.parse(line);
  } catch {
    return;
  }
  if (request.id === undefined) return;
  const { id, method, params } = request;
  if (method === "initialize") {
    send({ id, result: { protocolVersion: params?.protocolVersion ?? "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "aries-probe", version: "0.0.1" } } });
  } else if (method === "tools/list") {
    send({ id, result: { tools: [{ name: "aries_plugin_probe", description: "Return the ARIES probe marker. Takes no arguments.", inputSchema: { type: "object", properties: {} } }] } });
  } else if (method === "tools/call") {
    let before = -1;
    try {
      before = readdirSync(data).length;
      writeFileSync(`${data}/call-${process.pid}-${Date.now()}`, marker);
    } catch {}
    send({ id, result: { content: [{ type: "text", text: `aries-probe marker=${marker} cwd=${process.cwd()} data_files_before=${before}` }] } });
  } else if (method === "ping") {
    send({ id, result: {} });
  } else {
    send({ id, error: { code: -32601, message: `method ${method} not found` } });
  }
});
