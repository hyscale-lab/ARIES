package main

const (
	nextjsWave3Trial = "117ff592-3774-4ad2-bcc4-7d0e6292c096"
	nextjsWave4Trial = "3f615cbd-3dbc-4280-aa56-d4b46b1f5c61"
	nextjsWave5Trial = "8ecdcc26-f088-47c7-808a-620b04722c99"
)

const nextjsWave3Tools = `
test ! -e /tmp/warehouse-check
test ! -e /tmp/apply_patch
test ! -e /usr/bin/python3
ln -s /opt/aries-python/bin/python3 /usr/bin/python3
test "$(command -v python3)" = /usr/bin/python3
test "$(command -v patch)" = /usr/bin/patch
/usr/bin/env python3 -I -S -c 'import pathlib; print(pathlib.__file__)'
dpkg-query -W patch
`

const nextjsWave4PatchAlias = `
alias_dir=/root/.codex/tmp/arg0/codex-arg0bBgmul
target=/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
mkdir -p "$alias_dir"
test -x "$target"
ln -s "$target" "$alias_dir/apply_patch"
readlink "$alias_dir/apply_patch"
` + noShellPatch + "\n"

const nextjsLaterFinalCheck = `
test -s .next/BUILD_ID
for page in page pick-batches/page inventory/page shipments/page exceptions/page 'api/exceptions/[id]/resolve/route'; do
    test -s ".next/server/app/$page.js" || exit 1
done
test -x /root/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome
test -x /root/.cache/ms-playwright/chromium_headless_shell-1234/chrome-headless-shell-linux64/chrome-headless-shell
dpkg-query -W libnspr4 libnss3 libatk-bridge2.0-0 libgbm1
node -e 'const assert=require("assert/strict"); for (const name of ["playwright","playwright-core"]) assert.equal(require(process.argv[1]+"/node_modules/"+name+"/package.json").version,"1.62.1")' "$browser"
`

const nextjsWave3FinalCheck = `
app_tree="$(` + nextjsTreeDigest + `)"
printf 'final application tree %s\n' "$app_tree"
test "$app_tree" = 'b3070936097936d67028c6d2a4d9caafd6d3df2d58495842d58f41a863efa93a  -'
browser=/tmp/warehouse-check
sha256sum -c <<'ARIESSHA'
5890dff9956ea9d38e4ff12ee258645d324d41567541fb07f966d54bf652f829  PERFORMANCE.md
92c5fd1d189712e3c360ebba2b68071981ecb012952b2f281c98383957825fcb  /tmp/apply_patch
3f73391d6fad25555e638559804b10333269442c6595df5fae815b5c9a2098e8  /tmp/warehouse-check/audit-proxy.cjs
8d7006c4a80e6e75a4b13105b6bbba1615511f98359ad844a5cf935392b7f7b9  /tmp/warehouse-check/check.cjs
cf43a3c582849a965a2523588a0688c9b1a8fe226a9e291b8fb7cad0dc3a84aa  /tmp/warehouse-check/cold-load.cjs
ARIESSHA
test -s "$browser/audit-events.jsonl"
grep -Fq '"stage":"delivered"' "$browser/audit-events.jsonl"
sha256sum "$browser/inventory.png" "$browser/audit-events.jsonl"
`

const nextjsWave4FinalCheck = `
app_tree="$(` + nextjsTreeDigest + `)"
printf 'final application tree %s\n' "$app_tree"
test "$app_tree" = 'aa8b7bc659882df874f174b317a6676c875b17eefc75fa3c14f3cb8f5ed9b0bd  -'
browser=/tmp/warehouse-browser
sha256sum -c <<'ARIESSHA'
5d4a2b4dee7c0d502af47348805758be236f5437c7734c472312a57725b85cf9  /tmp/warehouse-browser/check.cjs
5b946326338c62bf4a8ddc9517749dc8d6ace6709f71a7d8c9262602be49715e  /tmp/warehouse-browser/controlled.cjs
ARIESSHA
node <<'NEXTJS'
const assert = require('node:assert/strict');
const fs = require('node:fs');
const root = '/tmp/warehouse-browser';
for (const phase of ['baseline','optimized']) {
  const rows = JSON.parse(fs.readFileSync(root + '/' + phase + '-timings.json', 'utf8'));
  assert(rows && typeof rows === 'object' && Object.keys(rows).length >= 5);
}
for (const name of ['dispatch','exceptions-resolved','exceptions','inventory','pick-batches-scored',
                    'pick-batches','shipments-planned','shipments']) {
  const baseline = fs.readFileSync(root + '/baseline-' + name + '.png');
  assert.equal(baseline.subarray(0,8).toString('hex'), '89504e470d0a1a0a');
  assert(baseline.equals(fs.readFileSync(root + '/optimized-' + name + '.png')), name);
}
console.log('verified eight source screenshot pairs');
NEXTJS
sha256sum "$browser"/{baseline,optimized}-timings.json "$browser"/{baseline,optimized}-*.png
`

const nextjsWave5FinalCheck = `
app_tree="$(` + nextjsTreeDigest + `)"
printf 'final application tree %s\n' "$app_tree"
test "$app_tree" = 'd2a91de2434dbf73a4b822c5b54d514cf3a92a3d26be52d3a179929cefe0bfde  -'
browser=/tmp/warehouse-check
sha256sum -c <<'ARIESSHA'
27e29dc797905227e08945e32558650ae21ab148082ec87af55c91965ba893de  /usr/local/bin/apply_patch
062cc9b277820a324cfbd9bdc7a2e4aaaf8a12b088b4b73676036030cdffe59a  /tmp/warehouse-check/check.cjs
2332222e3616f49c4302812b9f7edb4b50ed2ce04e7580cc0acfafc32e6515ba  /tmp/warehouse-check/chunks.cjs
8911367df1eb5148a9a6067e74dfaf3f4b59155b63bf4766292a431550f4fda6  /tmp/warehouse-check/contracts.cjs
ARIESSHA
grep -Fq 'Compiled successfully' /tmp/warehouse-final-build.log
node <<'NEXTJS'
const assert = require('node:assert/strict');
const fs = require('node:fs');
const sharp = require('sharp');
const root = '/tmp/warehouse-check';
for (const phase of ['baseline','final']) {
  const rows = JSON.parse(fs.readFileSync(root + '/' + phase + '.json', 'utf8'));
  assert(rows && typeof rows === 'object' && Object.keys(rows).length === 5);
}
(async () => {
  for (const name of ['dispatch','pick-batches','inventory','shipments','exceptions']) {
    const [base, final] = await Promise.all(['baseline','final'].map(phase =>
      sharp(root + '/' + phase + '-' + name + '.png').raw().toBuffer({resolveWithObject:true})));
    assert.deepEqual(base.info, final.info, name);
    assert(base.data.equals(final.data), name);
  }
  console.log('verified five source screenshot pairs');
})().catch(error => { console.error(error); process.exitCode = 1; });
NEXTJS
sha256sum "$browser"/{baseline,final}.json "$browser"/{baseline,final}-*.png /tmp/warehouse-final-build.log
`
