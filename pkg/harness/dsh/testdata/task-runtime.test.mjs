import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { once } from "node:events";
import { syncBuiltinESMExports } from "node:module";
import { setImmediate } from "node:timers/promises";
import { test } from "node:test";
import { Context } from "@deepseek-ai/cordis";
import { LocalSubprocessRuntime } from "./task-runtime.mjs";

async function fixture(t, startDrainer, command = "sleep 60") {
  const nativeSpawn = childProcess.spawn;
  let child;
  t.mock.method(childProcess, "spawn", (file, args, options) => {
    if (file === process.execPath && args[0] === "-e" && options.detached)
      return startDrainer(nativeSpawn, file, args, options);
    return child = nativeSpawn(file, args, options);
  });
  syncBuiltinESMExports();
  const ctx = new Context();
  await ctx.plugin(LocalSubprocessRuntime);
  const runtime = ctx.subprocess;
  const handle = runtime.spawn({
    argv: ["/bin/sh", "-c", command], cwd: "/tmp", graceMs: 100,
    stdio: { stdin: "ignore", stdout: "pipe", stderr: "pipe" },
  });
  t.after(async () => {
    // A successful handoff deliberately removes native ownership. The fixture
    // still owns this process group and must clean it up itself.
    try { process.kill(-child.pid, "SIGKILL"); }
    catch (error) { if (error.code !== "ESRCH") throw error; }
    await ctx.fiber.dispose();
    t.mock.restoreAll();
    syncBuiltinESMExports();
  });
  return { runtime, handle, child };
}

const startupFailure = error => {
  assert.match(error.message, /could not transfer background output/);
  assert.equal(error.cause.code, "ENOENT");
  return true;
};

test("asynchronous drainer spawn failure preserves readers and cleanup ownership", async t => {
  const { runtime, handle, child } = await fixture(t,
    (spawn, _file, args, options) => spawn("/aries-missing-output-drainer", args, options));
  await assert.rejects(handle.disown(), startupFailure);
  await setImmediate(); // An unhandled child error would terminate this helper.
  assert.equal(child.stdout.destroyed, false);
  assert.equal(child.stderr.destroyed, false);
  assert.equal(runtime.live.has(handle), true);
  // A repeated cleanup attempt must report the same failed handoff.
  await assert.rejects(handle.disownOutput(), startupFailure);
  handle.terminate();
  assert.equal(await handle.waitForExit(), true);
});

test("completion propagates a failed handoff and cancels its remaining process range", async t => {
  const { handle } = await fixture(t,
    (spawn, _file, args, options) => spawn("/aries-missing-output-drainer", args, options),
    "sleep 60 &");
  await assert.rejects(handle.done, startupFailure);
  assert.equal(await handle.waitForExit(), true);
});

test("original output readers remain open until the drainer confirms readiness", async t => {
  let drain;
  const started = Promise.withResolvers();
  const { runtime, handle, child } = await fixture(t, (spawn, file, args, options) => {
    drain = spawn(file, ["-e", `process.once("message",()=>{${args[1]}})`], options);
    started.resolve();
    return drain;
  });
  const transferred = handle.disown();
  await started.promise;
  await setImmediate();
  assert.equal(child.stdout.destroyed, false);
  assert.equal(child.stderr.destroyed, false);
  assert.equal(runtime.live.has(handle), true);
  const closed = once(drain, "close");
  drain.send("initialize");
  assert.equal(await transferred, true);
  assert.equal(child.stdout.destroyed, true);
  assert.equal(child.stderr.destroyed, true);
  assert.equal(runtime.live.has(handle), false);
  process.kill(-child.pid, "SIGKILL");
  await closed;
});
