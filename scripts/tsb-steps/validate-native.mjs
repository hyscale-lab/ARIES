// Run inside the pinned image, with this directory mounted at /plugin.
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
const state = fs.mkdtempSync("/tmp/aries-tsb-native-fixture-");
try {
  fs.cpSync("/plugin", path.join(state, "plugin"), { recursive: true });
  const configPath = path.join(state, "config.json");
  fs.writeFileSync(configPath, JSON.stringify({
    diagnostics: { enabled: true },
    plugins: { allow: ["aries-tsb-diagnostics"], load: { paths: [path.join(state, "plugin")] },
      entries: { "aries-tsb-diagnostics": { enabled: true } } },
  }));
  const result = spawnSync(process.execPath,
    ["/app/openclaw.mjs", "plugins", "inspect", "aries-tsb-diagnostics", "--runtime", "--json"],
    { env: { ...process.env, HOME: state, OPENCLAW_STATE_DIR: state, OPENCLAW_CONFIG_PATH: configPath },
      encoding: "utf8", timeout: 30000, maxBuffer: 4 * 1024 * 1024 });
  if (result.status !== 0) {
    console.error(result.stderr || result.error?.message || result.stdout);
    process.exitCode = 1;
  } else {
    const output = JSON.parse(result.stdout);
    const plugin = output.plugin;
    if (plugin?.status !== "loaded" || plugin?.imported !== true ||
        !output.services?.includes("aries-tsb-diagnostics") || output.diagnostics?.length) {
      throw new Error("Native plugin registration was not confirmed");
    }
    console.log(JSON.stringify({ plugin: plugin.id, status: plugin.status, imported: plugin.imported,
      services: output.services, typedHooks: output.typedHooks, diagnostics: output.diagnostics }, null, 2));
  }
} finally { fs.rmSync(state, { recursive: true, force: true }); }
