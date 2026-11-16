package dsh

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

//go:embed task-runtime.mjs
var taskRuntime string

// Adapt only teardown ownership. Native process results and explicit job_kill
// remain unchanged; releasing a registration does not invent a terminal outcome.
const adaptLifetime = `from pathlib import Path

def replace(path, before, after):
    text = path.read_text()
    if text.count(before) != 1:
        raise RuntimeError(f"DSH lifecycle adapter no longer matches {path}: {before!r}")
    path.write_text(text.replace(before, after))

helper = Path("/opt/aries/helper/node_modules/@deepseek-ai/dsh-ssh/lib/helper.js")
replace(helper, 'from "@deepseek-ai/dsh-subprocess-local";', 'from "./task-runtime.mjs";')
replace(helper, 'clearTimeout(record.expiry);\n\t\t\trecord.controller.abort(',
        'clearTimeout(record.expiry);\n\t\t\tif (this.closing && record.ordinary && (!record.ariesBackground || !await record.ordinary.disown())) record.ordinary.terminateForHostExit();\n\t\t\trecord.controller.abort(')
replace(helper, 'const done = ordinary.done.finally(() => {',
        'const done = ordinary.done.finally(async () => { await ordinary.disownOutput();')
replace(helper, 'if (method === "process.prepare")',
        'if (method === "process.preserve") { const id = processIdRequest.parse(raw).id; if (!processes.completed.has(id)) processes.record(id).ariesBackground = true; return null; }\n\t\tif (method === "process.prepare")')

root = Path("/opt/dsh/node_modules/@deepseek-ai")
remote = root / "dsh-subprocess-ssh/lib/index.js"
replace(remote, 'var RemoteProcess = class {',
        'var RemoteProcess = class {\n\tasync preserve() { await this.started; await this.ssh.request("process.preserve", { id: this.id }, z.null()); }')
shell = root / "dsh-bash-local/lib/index.js"
replace(shell, 'const proc = {', 'const proc = {\n\t\t\tpreserve: () => running?.preserve(),')

tool = root / "dsh-tool-bash/lib/index.js"
replace(tool, 'function processJob(start, outcome) {\n\tconst controller = new AbortController();',
        'function processJob(start, outcome) {\n\tconst started = Promise.withResolvers(); started.promise.catch(() => {});\n\tconst controller = new AbortController();')
replace(tool, 'let process;\n\treturn {',
        'let process;\n\treturn {\n\t\tpreserve: async () => { await (await started.promise).preserve(); },')
replace(tool, 'process = await start(controller.signal);',
        'process = await start(controller.signal); started.resolve(process);')
replace(tool, '} catch (error) {\n\t\t\t\treturn {\n\t\t\t\t\tstatus: controller.signal.aborted && process === void 0',
        '} catch (error) {\n\t\t\t\tstarted.reject(error);\n\t\t\t\treturn {\n\t\t\t\t\tstatus: controller.signal.aborted && process === void 0')
replace(tool, 'done: hooks.done,', 'done: hooks.done, preserve: hooks.preserve,')
replace(tool, 'async execute(args, exec) {\n\t\t\t\tconst standingPolicy',
        'async execute(args, exec) {\n\t\t\t\tconst execute = async () => {\n\t\t\t\tconst standingPolicy')
replace(tool, '\n\t\t\t},\n\t\t\tpresentCall: presentBashCall,',
        '\n\t\t\t\t}; const result = await execute(); if (result.kind === "background" || result.kind === "promoted") await jobs.ariesPreserve(result.jobId); return result;\n\t\t\t},\n\t\t\tpresentCall: presentBashCall,')

jobs = Path("/opt/dsh/node_modules/@deepseek-ai/dsh-jobs-local/lib/index.js")
replace(jobs, 'start(spec) {',
        'async ariesPreserve(id) { const job = this.store.get(id); if (job.status === "running") { await job.preserve(); job.ariesBackground = true; } }\n\tstart(spec) {')
replace(jobs, 'cancel: hooks.cancel.bind(hooks),',
        'cancel: hooks.cancel.bind(hooks), preserve: hooks.preserve,')
replace(jobs, 'cancelForTeardown(jobs, reason) {\n\t\tfor (const job of jobs) {',
        'cancelForTeardown(jobs, reason) {\n\t\tfor (const job of jobs) {\n\t\t\tif (job.ariesBackground && job.status === "running") { job.markSettled(); continue; }')
`

// Dockerfile installs the released CLI and matching SSH providers, then adapts
// task-process ownership. Both embedded adapter sources enter the image hash.
var Dockerfile = strings.NewReplacer(
	"ARIES_TASK_RUNTIME", base64.StdEncoding.EncodeToString([]byte(taskRuntime)),
	"ARIES_ADAPT_LIFETIME", base64.StdEncoding.EncodeToString([]byte(adaptLifetime)),
).Replace(`ARG BASE
FROM ${BASE}
ARG DSH_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends openssh-client python3 make g++ \
    && rm -rf /var/lib/apt/lists/*
RUN npm install --prefix /opt/dsh --no-audit --no-fund --save-exact \
    @deepseek-ai/dsh@${DSH_VERSION} @deepseek-ai/dsh-ssh@${DSH_VERSION} \
    @deepseek-ai/dsh-fs-ssh@${DSH_VERSION} @deepseek-ai/dsh-subprocess-ssh@${DSH_VERSION} \
    @deepseek-ai/dsh-sandbox-ssh@${DSH_VERSION}
RUN mkdir -p /opt/aries/helper && npm install --prefix /opt/aries/helper --no-audit --no-fund --save-exact \
    @deepseek-ai/dsh-ssh@${DSH_VERSION} @deepseek-ai/dsh-ptc-runtime-node@${DSH_VERSION} \
    && cp /usr/local/bin/node /opt/aries/helper/node \
    && printf '%s\n' 'import {createConnection} from "node:net"; const s=createConnection({path:process.argv[2],allowHalfOpen:true}); s.on("error",e=>{console.error(e.message);process.exitCode=1;process.stdin.destroy();}); process.stdin.pipe(s); s.pipe(process.stdout); s.on("end",()=>process.stdin.destroy());' > /opt/aries/helper/stream.mjs
RUN printf '%s' 'ARIES_TASK_RUNTIME' | base64 -d > /opt/aries/helper/node_modules/@deepseek-ai/dsh-ssh/lib/task-runtime.mjs \
    && printf '%s' 'ARIES_ADAPT_LIFETIME' | base64 -d | python3
RUN sha256sum /opt/aries/helper/node_modules/@deepseek-ai/dsh-ssh/lib/helper.js | cut -d' ' -f1 > /opt/aries/dsh-helper.sha256 \
    && tar -czf /opt/aries/dsh-helper.tar.gz -C /opt/aries/helper node node_modules stream.mjs \
    && rm -rf /opt/aries/helper
ENV PATH=/opt/dsh/node_modules/.bin:$PATH
`)

func LocalImage(base, version string) (string, error) {
	if err := containerimage.ValidatePinnedTagOnly(base); err != nil {
		return "", fmt.Errorf("DSH base image: %w", err)
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`).MatchString(version) {
		return "", fmt.Errorf("DSH version must be an explicit release")
	}
	sum := sha256.Sum256([]byte(base + "\n" + version + "\n" + Dockerfile))
	return fmt.Sprintf("aries-local/dsh:%s-%x", version, sum[:6]), nil
}

func BridgeImage(base, version string) (string, error) {
	image, err := LocalImage(base, version)
	if err != nil {
		return "", err
	}
	return image + "-bridge", nil
}
