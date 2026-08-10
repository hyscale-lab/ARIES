import {definePluginEntry} from "openclaw/plugin-sdk/plugin-entry";
import {registerSandboxBackend} from "openclaw/plugin-sdk/sandbox";
import {createAriesClient, createAriesFsBridge, runAriesShellCommand} from "./client.mjs";

const helperPath = "/opt/aries/openclaw/aries-e2b/helper.mjs";

export default definePluginEntry({
  id: "aries-e2b",
  name: "ARIES E2B Bridge",
  description: "Task-scoped access to an ARIES-owned sandbox.",
  configSchema: {
    safeParse(value: unknown) {
      const config = value as {address?: unknown; sandboxId?: unknown; tokenFile?: unknown; workdir?: unknown} | undefined;
      const valid = config && [config.address, config.sandboxId, config.tokenFile, config.workdir].every((entry) => typeof entry === "string" && entry.length > 0);
      return valid ? {success: true, data: config} : {success: false, error: {issues: [{path: [], message: "address, sandboxId, tokenFile, and workdir are required"}]}};
    },
    jsonSchema: {
      type: "object", additionalProperties: false, required: ["address", "sandboxId", "tokenFile", "workdir"],
      properties: {address: {type: "string"}, sandboxId: {type: "string"}, tokenFile: {type: "string"}, workdir: {type: "string"}},
    },
  },
  register(api) {
    if (api.registrationMode !== "full") return;
    const config = api.pluginConfig as {address: string; sandboxId: string; tokenFile: string; workdir: string};
    const client = createAriesClient(config);
    registerSandboxBackend("aries-e2b", {
      factory: async ({scopeKey, workspaceDir, cfg}) => {
        const workdir = config.workdir;
        return {
          id: "aries-e2b",
          runtimeId: `${config.sandboxId}:${scopeKey}`,
          runtimeLabel: config.sandboxId,
          workdir,
          env: cfg.docker.env,
          buildExecSpec: async ({command, workdir: requestedWorkdir, env, usePty}) => {
            if (usePty) throw new Error("ARIES E2B backend does not support PTY");
            return {
              argv: [helperPath, "exec", command, requestedWorkdir ?? workdir, JSON.stringify(env)],
              env: {...process.env, ARIES_E2B_ADDRESS: config.address, ARIES_E2B_SANDBOX_ID: config.sandboxId, ARIES_E2B_TOKEN_FILE: config.tokenFile},
              stdinMode: "pipe-closed",
            };
          },
          runShellCommand: async (params) => await runAriesShellCommand(client, workdir, params),
          createFsBridge: () => createAriesFsBridge(client, workdir),
        };
      },
      resolveWorkdir: () => config.workdir,
    });
  },
});
