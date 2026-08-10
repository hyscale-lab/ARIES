#!/usr/bin/env node
import {createAriesClient} from "./client.mjs";

const controller = new AbortController();
let signalExit = 0;
for (const [name, code] of [["SIGTERM", 143], ["SIGINT", 130]]) {
  process.once(name, () => { signalExit = code; controller.abort(); setImmediate(() => process.exit(code)); });
}

try {
  if (process.argv[2] !== "exec" || process.argv.length !== 6) throw new Error("invalid ARIES E2B helper invocation");
  const client = createAriesClient({
    address: process.env.ARIES_E2B_ADDRESS,
    sandboxId: process.env.ARIES_E2B_SANDBOX_ID,
    tokenFile: process.env.ARIES_E2B_TOKEN_FILE,
  });
  const env = JSON.parse(process.argv[5]);
  const result = await client.startProcess({
    command: process.argv[3], workdir: process.argv[4], env, signal: controller.signal,
    onStdout: (chunk) => process.stdout.write(chunk),
    onStderr: (chunk) => process.stderr.write(chunk),
  });
  if (result.error) throw new Error(result.error);
  process.exitCode = result.code;
} catch (error) {
  if (signalExit) process.exit(signalExit);
  process.stderr.write(String(error?.message ?? error) + "\n");
  process.exitCode = 125;
}
