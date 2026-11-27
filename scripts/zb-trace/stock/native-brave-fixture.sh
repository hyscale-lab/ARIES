# Sourced by supplemental-fixture.sh for recordings with native process or image tools.
if [[ $stage == upload ]]; then
    upload "$here/../../../cmd/aries-agentenv-replay/source-tools.mjs" /workload/rootfs/tmp/zb-source-tools.mjs
    upload "$fixture/source-tools.json" /workload/rootfs/tmp/zb-source-tools.json
    if [[ $task == resume-homepage ]]; then
        upload "$fixture/playwright-wheels.tar" /tmp/resume-wheels.tar
        guest sh -ec 'mkdir /workload/rootfs/tmp/resume-wheels; tar xf /tmp/resume-wheels.tar -C /workload/rootfs/tmp/resume-wheels; rm /tmp/resume-wheels.tar'
    fi
else
    guest sh -ec 'runc exec -d --env NODE_NO_WARNINGS=1 zb-workload node /tmp/zb-source-tools.mjs serve /tmp/zb-source-tools.json </dev/null >/tmp/zb-source-node.log 2>&1'
    guest runc exec zb-workload sh -ec 'for n in $(seq 90); do test ! -S /tmp/zb-source-tools.sock || exit 0; sleep 1; done; exit 1'
fi
if [[ $stage == ready && $task == resume-homepage ]]; then
    guest runc exec zb-workload sh -ec '
        cd /tmp_workspace
        echo "6627d26e06c5b77e15bbe159da28eec79eacade90d4a078224883176c9ac986e  .wildclaw/run-warmup.sh
fac255b8da119f0a114cb1f7e09133ddeb76811b77a5d619ebe0ade5b9b33bc5  ref_resume.pdf
7488c73f54545244b30d2ebc025c7c2924d39a423c5f9b5f9f3da16dc5f29bc0  ref_template_screenshot.png" | sha256sum -c -
        PIP_NO_INDEX=1 PIP_FIND_LINKS=/tmp/resume-wheels bash .wildclaw/run-warmup.sh
        test -f /tmp/wildclaw-harbor-warmup.done
        python3 -m playwright --version | grep -F 1.61.0
        /root/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome --version
        rm -r /tmp/resume-wheels
    ' >"$run/native-browser-ready.txt"
    warmup=none
fi
