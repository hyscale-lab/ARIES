package main

import "strings"

const nextjsWave2Trial = "09288c37-6d48-4073-a05a-1d5770fecef0"

func nextjsSourceSetup(trial string) (taskSetup, bool) {
	setup := nextjsSetup
	switch trial {
	case nextjsTrial:
		return setup, true
	case nextjsWave2Trial:
		// This source writes a Python patch helper and invokes it via env.
		setup.initialCheck = nextjsWave2Date + strings.NewReplacer(
			"2026-08-31T10:08:34.000Z", "2026-08-31T08:03:11.700Z",
			"2026-08-31T10:08:34Z", "2026-08-31T08:03:11.700Z",
			"20260831T100834Z", "20260831T080311Z",
		).Replace(setup.initialCheck) + nextjsWave2Python
		return setup, true
	default:
		return taskSetup{}, false
	}
}

const nextjsWave2Date = `test ! -e /etc/profile.d/zz-aries-nextjs-npm.sh
printf '%s\n' 'export npm_config_before=2026-08-31T08:03:11.700Z' > /etc/profile.d/zz-aries-nextjs-npm.sh
. /etc/profile.d/zz-aries-nextjs-npm.sh
sed -i 's/20260831T100834Z/20260831T080311Z/g' /etc/apt/sources.list.d/debian.sources
`

const nextjsWave2Python = `
test ! -e /tmp/apply_patch
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
