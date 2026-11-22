package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed financial_apt.json
var financialAptJSON []byte

type financialAptProfile struct {
	Snapshot string            `json:"snapshot"`
	Packages map[string]string `json:"packages"`
}

var financialAptTrials = func() map[string]financialAptProfile {
	var trials map[string]financialAptProfile
	if err := json.Unmarshal(financialAptJSON, &trials); err != nil {
		panic(err)
	}
	return trials
}()

// The source runs apt update and install itself. The date-addressed signed
// repository preserves package availability while keeping those operations live.
func financialAptSetup(profile financialAptProfile) string {
	return strings.ReplaceAll(`
test -f /etc/apt/sources.list.d/ubuntu.sources
test -s /usr/share/keyrings/ubuntu-archive-keyring.gpg
cat > /etc/apt/sources.list.d/ubuntu.sources <<'SOURCES'
Types: deb
URIs: https://snapshot.ubuntu.com/ubuntu/STAMP/
Suites: noble noble-updates noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
Check-Valid-Until: no
SOURCES
cat /etc/apt/sources.list.d/ubuntu.sources`, "STAMP", profile.Snapshot)
}

func financialAptCheck(profile financialAptProfile) string {
	packages, err := json.Marshal(profile.Packages)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`
python3 - <<'APT'
import json, subprocess
versions = json.loads('%s')
actual = subprocess.check_output(["dpkg-query","-W","-f=" + "${Package}\t${Version}\n",*versions],text=True)
assert dict(line.split("\t") for line in actual.splitlines()) == versions
print("Verified", len(versions), "source-observed APT package versions")
APT`, packages)
}
