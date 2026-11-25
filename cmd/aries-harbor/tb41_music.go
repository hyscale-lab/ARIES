package main

// musicHarmonySetup stages wheels only; the recorded pip install remains live.
// The wheel set is a dated reconstruction, not the recording's package lock.
var musicHarmonySetup = taskSetup{
	packagesSHA:   "324d150ec405b6e41986dc59696b70aab6c1525e69c158eb89a3082fc88b717c",
	packagesField: "music_wheels_archive_sha256",
	output:        "/app/harmony.mxl",
	inputCheck: `echo '3b20d23701545567237729620db3ec9ee4ec7416bb37f15c12d10c6bb1b1ed3a  /app/Harmony.pdf' | sha256sum -c -`,
	initialCheck: `test ! -e /app/harmony.mxl
if command -v pdftoppm; then exit 1; fi
if command -v apply_patch; then exit 1; fi
python - <<'ARIESPY'
import importlib.util, sys
assert sys.version_info[:2] == (3, 11)
assert all(importlib.util.find_spec(x) is None for x in ('fitz','music21','PIL'))
ARIESPY
(cd /opt/zb-music/wheels && sha256sum -c ../SHA256SUMS)
test ! -e /etc/pip.conf
test ! -e /tmp/zb-music-pip-report.json
cat > /etc/pip.conf <<'ARIESPIP'
[install]
no-index = true
find-links = file:///opt/zb-music/wheels
report = /tmp/zb-music-pip-report.json
ARIESPIP
sha256sum /etc/pip.conf`,
}
