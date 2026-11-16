// The task container owns background work through evaluation. Keep DSH's native
// subprocess transport, output and cancellation, but release ordinary processes
// when their command completes or the helper's access is revoked.
import { spawn } from "node:child_process";
import { LocalSubprocessRuntime as NativeRuntime } from "@deepseek-ai/dsh-subprocess-local";

export class LocalSubprocessRuntime extends NativeRuntime {
  selectContainmentMode(kind) {
    // The task container is the process boundary; no host systemd scope is owned.
    return kind === "ordinary" ? "fallback" : super.selectContainmentMode(kind);
  }

  spawn(spec) {
    const cancellation = new AbortController();
    const released = Promise.withResolvers();
    let child, handle, completed = false, transferred = false, cancelling = false;
    const onAbort = () => {
      cancelling = true;
      cancellation.abort(spec.signal.reason);
    };
    spec.signal?.addEventListener("abort", onAbort, { once: true });
    if (spec.signal?.aborted) onAbort();
    const internals = this.internals;
    this.internals = { ...internals, spawn: (...args) => (child = spawn(...args)) };
    try {
      handle = super.spawn({ ...spec, signal: cancellation.signal });
    } catch (error) {
      spec.signal?.removeEventListener("abort", onAbort);
      throw error;
    } finally {
      this.internals = internals;
    }
    const terminate = handle.terminate;
    const terminateForHostExit = handle.terminateForHostExit;
    const waitForExit = handle.waitForExit;
    let outputTransfer;
    handle.disownOutput = () => {
      if (outputTransfer !== undefined) return outputTransfer;
      if (cancelling) return Promise.resolve();
      outputTransfer = (async () => {
        // A descendant can use setsid and keep these pipes open after the
        // original process group disappears. Transfer every remaining reader.
        const readers = [child.stdout, child.stderr].map(stream => stream?.readable && !stream.destroyed ? stream : "ignore");
        const fds = readers.flatMap((stream, index) => stream === "ignore" ? [] : [index === 0 ? 0 : 3]);
        if (fds.length === 0) return;
        // Keep the child alive on IPC until both sides accept the handoff,
        // including when the pipes reach EOF during startup.
        const drain = spawn(process.execPath, ["-e", `const readers=${JSON.stringify(fds)}.map(fd=>new (require("node:net").Socket)({fd,readable:true,writable:false})); process.once("message",()=>{for(const reader of readers) reader.resume(); process.disconnect();}); process.send("ready");`], {
          stdio: [readers[0], "ignore", "ignore", readers[1], "ipc"], detached: true,
        });
        const closed = new Promise(resolve => drain.once("close", resolve));
        try {
          await new Promise((resolve, reject) => {
            // A failed spawn emits error asynchronously, even without a PID.
            // Retain this listener through close, including after a ready reply.
            const onError = error => reject(error);
            const onExit = (code, signal) => reject(new Error(`output drainer exited before readiness (${signal ?? code})`));
            drain.on("error", onError);
            drain.once("exit", onExit);
            drain.once("message", message => {
              if (message !== "ready") return reject(new Error("invalid output drainer readiness reply"));
              try { drain.send("accepted", error => error ? reject(error) : resolve()); }
              catch (error) { reject(error); }
            });
            closed.then(() => {
              drain.removeListener("error", onError);
              drain.removeListener("exit", onExit);
            });
          });
        } catch (error) {
          if (drain.pid !== undefined) {
            drain.kill("SIGKILL");
            await closed;
          }
          throw new Error("could not transfer background output to the task sandbox", { cause: error });
        }
        drain.unref();
      })();
      return outputTransfer;
    };
    const transfer = async (closing) => {
      // An explicit cancellation still owns its whole range, including KILL
      // escalation after the direct parent has exited.
      if (cancelling) return false;
      await handle.disownOutput();
      if (cancelling) return false;
      if (closing && !completed && !transferred) {
        child.stdin?.destroy();
        child.stdout?.destroy();
        child.stderr?.destroy();
        handle.control?.destroy();
        child.unref();
      }
      transferred = true;
      spec.signal?.removeEventListener("abort", onAbort);
      this.live.delete(handle);
      released.resolve();
      return true;
    };
    handle.disown = () => transfer(true);
    handle.terminate = () => {
      if (transferred) return;
      cancelling = true;
      terminate();
      released.resolve();
    };
    handle.terminateForHostExit = () => {
      if (transferred) return;
      cancelling = true;
      terminateForHostExit();
      released.resolve();
    };
    handle.waitForExit = async (signal) => {
      if (transferred) return true;
      if (cancelling) return waitForExit(signal);
      if (signal?.aborted) return false;
      const interrupted = Promise.withResolvers();
      const abort = () => interrupted.resolve(false);
      signal?.addEventListener("abort", abort, { once: true });
      try {
        const observed = await Promise.race([released.promise.then(() => true), interrupted.promise]);
        return observed && (cancelling ? waitForExit(signal) : true);
      } finally {
        signal?.removeEventListener("abort", abort);
      }
    };
    handle.done = handle.done.then(async outcome => {
      completed = true;
      if (!cancelling) {
        try { await transfer(false); }
        catch (error) { handle.terminate(); throw error; }
      } else released.resolve();
      return outcome;
    }, error => {
      cancelling = true;
      spec.signal?.removeEventListener("abort", onAbort);
      released.resolve();
      throw error;
    });
    handle.done.catch(() => {});
    return handle;
  }
}
