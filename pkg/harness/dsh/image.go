package dsh

import (
	"crypto/sha256"
	"fmt"
	"regexp"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

// Dockerfile installs the released CLI and its matching native SSH providers.
// The helper archive contains no model credentials or task-specific state.
const Dockerfile = `ARG BASE
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
    && printf '%s\n' 'import {createConnection} from "node:net"; const s=createConnection({path:process.argv[2],allowHalfOpen:true}); s.on("error",e=>{console.error(e.message);process.exitCode=1;process.stdin.destroy();}); process.stdin.pipe(s); s.pipe(process.stdout); s.on("end",()=>process.stdin.destroy());' > /opt/aries/helper/stream.mjs \
    && sha256sum /opt/aries/helper/node_modules/@deepseek-ai/dsh-ssh/lib/helper.js | cut -d' ' -f1 > /opt/aries/dsh-helper.sha256 \
    && tar -czf /opt/aries/dsh-helper.tar.gz -C /opt/aries/helper node node_modules stream.mjs \
    && rm -rf /opt/aries/helper
ENV PATH=/opt/dsh/node_modules/.bin:$PATH
`

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
