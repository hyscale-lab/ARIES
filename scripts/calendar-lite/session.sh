#!/bin/bash
# Inside the disposable guest, with the prepared backing workspace.
set -eu
chown -R root:root /srv/calendar
if [[ -d /srv/calendar/.wildclaw ]]; then mv /srv/calendar/.wildclaw /opt/pilot/warmup; fi
mkdir /tmp_workspace
cat > /opt/pilot/calendar.toml <<'TOML'
[filesystem]
read = [".", "/usr", "/bin", "/lib", "/lib64", "/etc", "/run", "/root/USER.md"]
write = [".", "/tmp_workspace"]
deny = [".env", ".env.*", "*.pem", "*.key", ".ssh", ".aws", ".gnupg", ".git-credentials", "Library/Keychains", "Keychains", "id_rsa", "id_ed25519"]
TOML
deltabox session start /srv/calendar --policy /opt/pilot/calendar.toml --fs-mode cbcow-through --require-fs-mode --json > /run/lite-session.json
python3 - <<'PY'
import json, os, subprocess
from pathlib import Path
s=json.load(open('/run/lite-session.json'))
assert s['resolved_fs_mode']=='cbcow-through' and os.path.ismount(s['mountpoint']), s
Path('/run/lite-id').write_text(s['session_id'])
subprocess.run(['mount', '--bind', s['mountpoint'], '/tmp_workspace'], check=True)
PY
mkdir -p /run/calendar-probe
systemd-run --quiet --unit=lite-token --setenv=HOME=/root deltabox exec "$(cat /run/lite-id)" \
  --project /srv/calendar --fs-read-allow /opt/pilot/token.py --fs-write-allow /run/calendar-probe \
  -- python3 /opt/pilot/token.py serve
for attempt in {1..100}; do
  if [[ -S /run/calendar-probe/token.sock ]]; then exit 0; fi
  sleep .1
done
journalctl -u lite-token --no-pager >&2
exit 1
