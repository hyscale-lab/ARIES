import fs from "node:fs/promises";
import path from "node:path";

const tokenHeader = "X-Access-Token";
const sandboxHeader = "E2b-Sandbox-Id";

export function createAriesClient(config) {
  if (!config || typeof config.address !== "string" || typeof config.sandboxId !== "string" || typeof config.tokenFile !== "string") {
    throw new Error("invalid ARIES E2B plugin configuration");
  }
  const address = config.address.replace(/\/+$/, "");
  async function request(route, options = {}) {
    const token = (await fs.readFile(config.tokenFile, "utf8")).trim();
    if (!token) throw new Error("ARIES E2B access token is empty");
    const headers = new Headers(options.headers);
    headers.set(sandboxHeader, config.sandboxId);
    headers.set(tokenHeader, token);
    return await fetch(address + route, {...options, headers});
  }
  async function json(route, payload, signal) {
    const response = await request(route, {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify(payload),
      signal,
    });
    if (!response.ok) throw new Error(await responseError(response));
    return await response.json();
  }
  return {
    request,
    json,
    async startProcess({command, args = [], workdir, env = {}, signal, onStdout, onStderr}) {
      const response = await request("/v1/process/start", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({process: {cmd: "/bin/bash", args: ["-lc", command, "aries-e2b", ...args], cwd: workdir, envs: env}}),
        signal,
      });
      if (!response.ok) throw new Error(await responseError(response));
      if (!response.body) throw new Error("ARIES Process.Start response has no body");
      let pending = "";
      let started = false;
      let ended = false;
      let result;
      const decoder = new TextDecoder();
      for await (const chunk of response.body) {
        pending += decoder.decode(chunk, {stream: true});
        let newline;
        while ((newline = pending.indexOf("\n")) >= 0) {
          const line = pending.slice(0, newline);
          pending = pending.slice(newline + 1);
          if (line) ({started, ended, result} = consumeProcessEvent(line, {started, ended, result, onStdout, onStderr}));
        }
      }
      pending += decoder.decode();
      if (pending.trim()) ({started, ended, result} = consumeProcessEvent(pending, {started, ended, result, onStdout, onStderr}));
      if (!started || !ended || !result) throw new Error("incomplete ARIES Process.Start stream");
      return result;
    },
    async readFile(filePath, signal) {
      const response = await request("/v1/files?path=" + encodeURIComponent(filePath), {method: "GET", signal});
      if (!response.ok) throw new Error(await responseError(response));
      return Buffer.from(await response.arrayBuffer());
    },
    async writeFile(filePath, data, signal) {
      const response = await request("/v1/files?path=" + encodeURIComponent(filePath), {
        method: "POST", headers: {"Content-Type": "application/octet-stream"}, body: data, signal,
      });
      if (!response.ok) throw new Error(await responseError(response));
    },
  };
}

function consumeProcessEvent(line, state) {
  const message = JSON.parse(line);
  const event = message?.event;
  if (event?.start) {
    if (state.started || state.ended) throw new Error("invalid duplicate ARIES process start event");
    state.started = true;
  } else if (event?.data) {
    if (!state.started || state.ended) throw new Error("ARIES process data outside active stream");
    if (event.data.stdout) state.onStdout?.(Buffer.from(event.data.stdout, "base64"));
    if (event.data.stderr) state.onStderr?.(Buffer.from(event.data.stderr, "base64"));
  } else if (event?.end) {
    if (!state.started || state.ended) throw new Error("invalid ARIES process end event");
    state.ended = true;
    state.result = {code: event.end.exitCode, error: event.end.error};
  } else {
    throw new Error("unknown ARIES process event");
  }
  return state;
}

async function responseError(response) {
  const text = await response.text();
  try { return JSON.parse(text).error || `ARIES bridge returned HTTP ${response.status}`; }
  catch { return text || `ARIES bridge returned HTTP ${response.status}`; }
}

export async function runAriesShellCommand(client, workdir, params) {
  if (params.stdin !== undefined && Buffer.byteLength(params.stdin) !== 0) {
    throw new Error("ARIES E2B backend does not support stdin");
  }
  const stdout = [];
  const stderr = [];
  const result = await client.startProcess({
    command: params.script,
    args: params.args ?? [],
    workdir,
    signal: params.signal,
    onStdout: (chunk) => stdout.push(chunk),
    onStderr: (chunk) => stderr.push(chunk),
  });
  const output = {stdout: Buffer.concat(stdout), stderr: Buffer.concat(stderr), code: result.code};
  if (result.error) throw new Error(result.error);
  if (result.code !== 0 && !params.allowFailure) throw new Error(output.stderr.toString("utf8").trim() || `command exited with code ${result.code}`);
  return output;
}

export function createAriesFsBridge(client, workdir) {
  const resolvePath = ({filePath, cwd}) => {
    const containerPath = path.posix.resolve(cwd ?? workdir, filePath);
    return {relativePath: path.posix.relative(workdir, containerPath), containerPath};
  };
  const target = (filePath, cwd) => resolvePath({filePath, cwd}).containerPath;
  return {
    resolvePath,
    readFile: async ({filePath, cwd, signal}) => await client.readFile(target(filePath, cwd), signal),
    writeFile: async ({filePath, cwd, data, encoding, signal}) => {
      const content = Buffer.isBuffer(data) ? data : Buffer.from(data, encoding);
      await client.writeFile(target(filePath, cwd), content, signal);
    },
    mkdirp: async ({filePath, cwd, signal}) => { await client.json("/v1/filesystem/make-dir", {path: target(filePath, cwd)}, signal); },
    remove: async ({filePath, cwd, signal}) => { await client.json("/v1/filesystem/remove", {path: target(filePath, cwd)}, signal); },
    rename: async ({from, to, cwd, signal}) => { await client.json("/v1/filesystem/move", {source: target(from, cwd), destination: target(to, cwd)}, signal); },
    stat: async ({filePath, cwd, signal}) => {
      try {
        const response = await client.json("/v1/filesystem/stat", {path: target(filePath, cwd)}, signal);
        const entry = response.entry;
        return {type: entry.type === "file" || entry.type === "directory" ? entry.type : "other", size: entry.size, mtimeMs: entry.modifiedAt ? Date.parse(entry.modifiedAt) : 0};
      } catch (error) {
        if (/not exist|not found/i.test(String(error))) return null;
        throw error;
      }
    },
  };
}
