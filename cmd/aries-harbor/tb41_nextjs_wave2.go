package main

import "strings"

const nextjsWave2Trial = "09288c37-6d48-4073-a05a-1d5770fecef0"

func nextjsSourceSetup(trial string) (taskSetup, bool) {
	switch trial {
	case nextjsTrial:
		return nextjsSetup, true
	case nextjsWave2Trial:
		setup := nextjsDatedSetup("2026-08-31T08:03:11.700Z", "20260831T080311Z")
		// This source writes a Python patch helper and invokes it via env.
		setup.initialCheck += nextjsWave2Python
		return setup, true
	case nextjsWave3Trial:
		setup := nextjsDatedSetup("2026-08-31T08:31:40.614Z", "20260831T083140Z")
		setup.packagesSHA = "2d1665bc3a472a5e895657f41612ecf4e40e4349757d6481a19420b57b8146d0"
		setup.packagesField = "debian_patch_archive_sha256"
		setup.output = "PERFORMANCE.md"
		setup.initialCheck += nextjsWave3Tools
		return setup, true
	case nextjsWave4Trial:
		setup := nextjsDatedSetup("2026-08-31T09:10:10.480Z", "20260831T091010Z")
		setup.initialCheck += nextjsWave4PatchAlias + "\napt-get -o APT::Update::Error-Mode=any update -qq\n" + nextjsChromiumArchiveCache
		return setup, true
	case nextjsWave5Trial:
		return nextjsDatedSetup("2026-08-31T07:12:18.292Z", "20260831T071218Z"), true
	default:
		return taskSetup{}, false
	}
}

func nextjsDatedSetup(npmCutoff, aptSnapshot string) taskSetup {
	setup := nextjsSetup
	dateCheck := strings.NewReplacer(
		"2026-08-31T08:03:11.700Z", npmCutoff,
		"20260831T080311Z", aptSnapshot,
	).Replace(nextjsWave2Date)
	// npm prints its Date setting at whole-second precision.
	setup.initialCheck = dateCheck + strings.NewReplacer(
		"2026-08-31T10:08:34.000Z", npmCutoff[:19]+".000Z",
		"2026-08-31T10:08:34Z", npmCutoff,
		"20260831T100834Z", aptSnapshot,
	).Replace(setup.initialCheck)
	return setup
}

const nextjsWave2Date = `test ! -e /etc/profile.d/zz-aries-nextjs-npm.sh
printf '%s\n' 'export npm_config_before=2026-08-31T08:03:11.700Z' > /etc/profile.d/zz-aries-nextjs-npm.sh
. /etc/profile.d/zz-aries-nextjs-npm.sh
sed -i 's/20260831T100834Z/20260831T080311Z/g' /etc/apt/sources.list.d/debian.sources
`

const nextjsChromiumArchiveCache = `
# Keep signed archives available across the recorded apt update's docker-clean hook.
mkdir -p /var/cache/aries-apt/archives/partial
printf '%s\n' 'Dir::Cache::archives "/var/cache/aries-apt/archives";' > /etc/apt/apt.conf.d/99aries-cache
apt-get -qq -y --no-install-recommends --download-only install xvfb fonts-noto-color-emoji fonts-unifont libfontconfig1 libfreetype6 xfonts-scalable fonts-liberation fonts-ipafont-gothic fonts-wqy-zenhei fonts-tlwg-loma-otf fonts-freefont-ttf libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcairo2 libcups2 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0 libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2
test -s "$(find /var/cache/aries-apt/archives -name 'libnspr4_*.deb' -print -quit)"
`

const nextjsWave2Python = `
test ! -e /tmp/apply_patch
# The original first Chromium launch found GLib and then failed on libnspr4.
apt-get -o APT::Update::Error-Mode=any update -qq
DEBIAN_FRONTEND=noninteractive apt-get -qq -y --no-install-recommends install libglib2.0-0
` + nextjsChromiumArchiveCache + `
test -e /usr/lib/x86_64-linux-gnu/libglib-2.0.so.0
if ldconfig -p | grep -q 'libnspr4\.so'; then exit 1; fi
test ! -e /usr/local/bin/python3
ln -s /opt/aries-python/bin/python3 /usr/local/bin/python3
/usr/bin/env python3 -I -S -c 'import pathlib; print(pathlib.__file__)'
`

const nextjsWave2FinalCheck = `
app_tree="$(` + nextjsTreeDigest + `)"
test "$app_tree" = '55faabab688b6eede504ae624c2a1b81b0add0c913e4c2f9bd55918123686da9  -'
printf 'final application tree %s\n' "$app_tree"
sha256sum -c <<'ARIESSHA'
012cdb374ff278ed3ffb84170cc5efa4ce424de3cef7dc0ae5951ea44a9a93ac  /tmp/apply_patch
a5637d82f64c7faab4df04f503fb61004a56c35f9e879433afa48be7a098091f  /tmp/warehouse-browser/check.cjs
5f7bb9ce64cfac4ae1247c6389353f98d1519e95887d68bad3c428bc5de01a81  /tmp/warehouse-browser/streaming.cjs
ARIESSHA
test -s .next/BUILD_ID
grep -Fq 'Compiled successfully' /tmp/warehouse-optimized-build.log
grep -Fq 'Finalizing page optimization' /tmp/warehouse-optimized-build.log
for page in page pick-batches/page inventory/page shipments/page exceptions/page 'api/exceptions/[id]/resolve/route'; do
    test -s ".next/server/app/$page.js" || exit 1
done
test -x /root/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome
test -x /root/.cache/ms-playwright/chromium_headless_shell-1234/chrome-headless-shell-linux64/chrome-headless-shell
dpkg-query -W libnspr4 libnss3 libatk-bridge2.0-0 libgbm1
node <<'NEXTJS'
const assert = require('node:assert/strict');
const fs = require('node:fs');
const json = path => JSON.parse(fs.readFileSync(path, 'utf8'));
for (const name of ['playwright', 'playwright-core']) {
  assert.equal(json('/tmp/warehouse-browser/node_modules/' + name + '/package.json').version, '1.62.1');
}
const routes = ['/', '/pick-batches', '/inventory', '/shipments', '/exceptions'];
const baseline = json('/tmp/baseline-results.json');
const optimized = json('/tmp/optimized-results.json');
for (const phase of [baseline, optimized]) {
  assert.deepEqual(phase.map(row => row.route), routes);
  for (const {route, titleMs, rowsMs, text} of phase) {
    assert(Number.isFinite(titleMs) && titleMs >= 0, route);
    assert(Number.isFinite(rowsMs) && rowsMs >= titleMs, route);
    assert(text.trim(), route);
  }
}
for (const [i, row] of optimized.entries()) {
  assert.deepEqual(row.modules, [], row.route);
  assert.equal(row.text, baseline[i].text);
  assert.deepEqual(row.outputs, baseline[i].outputs);
  const suffix = row.route.replaceAll('/', '_');
  for (const layout of ['', '-mobile']) {
    const screenshot = fs.readFileSync('/tmp/optimized-' + suffix + layout + '.png');
    assert.equal(screenshot.subarray(0, 8).toString('hex'), '89504e470d0a1a0a');
    assert(screenshot.equals(fs.readFileSync('/tmp/baseline-' + suffix + layout + '.png')), row.route);
  }
}
console.log(JSON.stringify({validated_routes: routes, matching_baseline_screenshots: true}));
NEXTJS
sha256sum /tmp/{baseline,optimized}-results.json /tmp/{baseline,optimized}-*.png /tmp/warehouse-optimized-build.log /tmp/warehouse-streaming-server.log
`
