package main

const soundCascadeTask = "sound-change-cascade"

var soundCascadeSetup = taskSetup{
	output: "rules.json",
	inputCheck: `sha256sum -c <<'SOUNDINPUT'
edde5b508f9a631885ed84b9de5738ba55be8ca4311586cef62a2e51d1911465  engine/apply.py
ace3e6524914e18aa33dc5aee31ba6c70232616f89cfd8958aad6ec4ac385f4a  engine/example_rules.json
261f825d9bb42c202840dcf7256998f2103ba4511bbbb6a6dca3af99af057b3d  data/train.tsv
SOUNDINPUT
`,
	initialCheck: `test ! -e rules.json && test ! -e ordering.txt
test "$(wc -l < data/train.tsv)" -eq 780
test "$(command -v python)" = /usr/local/bin/python
test "$(command -v git)" = /usr/bin/git
python -c 'import importlib.util,sys; assert sys.version_info[:2] == (3,12); assert not any(importlib.util.find_spec(p) for p in ("rapidfuzz","numpy"))'
python -m pip --version | grep '^pip 25\.0\.1 '
git --version
command -v g++ patch
(cd /opt/zb-sound/wheels && sha256sum -c SHA256SUMS)
` + noShellPatch,
}

const soundCascadeFinal = `
test -s ordering.txt
sha256sum ordering.txt
predictions=$(mktemp)
trap 'rm -f "$predictions"' EXIT
python -B engine/apply.py rules.json ordering.txt data/train.tsv "$predictions"
cmp "$predictions" data/train.tsv
printf 'official engine equality: 780/780 pairs\n'
`
