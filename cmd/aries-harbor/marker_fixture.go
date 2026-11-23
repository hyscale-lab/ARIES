package main

import "fmt"

// These source-bound Ubuntu sessions operate on the same published fluorescent-tag fixture.
const markerTask = "dna-assembly"

var markerTrials = map[string]struct{ stamp, outputSHA string }{
	"ddf7548f-de77-4e90-aa31-ae1f86311601": {"20260831T212241Z", "6840d29c7c6f5ab1c28b3be5cce0bada7a74cf978a496e6d5a632191922279fc"},
	"8968e176-fa1f-4d6a-8e5a-3ebd7cc0b8de": {"20260831T202034Z", "fee914d1d9a30cc385d25613ae53c0f3777ac79e4bc4ca01aedf66800fe18059"},
	"19addf4a-2f79-4fc8-b873-3362c590da95": {"20260831T210301Z", "20e885eda4350dd7b2ac0d81853ba458b31e1749b460c0d5813bc79d8f5f6aa7"},
	"ba8d88f7-b8ea-4a81-a197-a10e8126f9f8": {"20260831T202342Z", "205e642d707b27a2277ace694996b49ef0b3bd8ee1397cccb0614ab32dc9ff3c"},
	"ed31bf8e-3dff-4080-bb5e-15c54bae728a": {"20260831T210255Z", "97f671d958057529d52b7c7ec0b222b4c4fa07029ae6afdd27f5b05b5fb0c17e"},
}

var markerSetup = taskSetup{
	packagesSHA: sparqlSetup.packagesSHA,
	output:      "primers.fasta",
	inputCheck:  "echo 'ebdc361b5fba28eb0a2c7a8a8a444ccb12cc842c8cbc6df5a2c24254f130736f  sequences.fasta' | sha256sum -c -",
}

func markerReady(stamp string) string {
	return financialAptSetup(financialAptProfile{Snapshot: stamp}) + `
test ! -e primers.fasta
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends gcc make >/dev/null
test -z "$(command -v oligotm)"
apt-cache policy primer3 | grep -F 'Candidate: 2.6.1-4'
command -v gcc make curl git python3`
}

func markerFinal(trial string) string {
	packageCheck := `test "$(dpkg-query -W -f='${Version}' primer3)" = 2.6.1-4`
	switch trial {
	case "ed31bf8e-3dff-4080-bb5e-15c54bae728a":
		packageCheck = `test "$(dpkg-deb -f /tmp/primer-design-tools/primer3_*.deb Version)" = 2.6.1-4 && test -x /tmp/primer-design-tools/primer3/usr/bin/oligotm`
	case "ba8d88f7-b8ea-4a81-a197-a10e8126f9f8":
		packageCheck = `test "$(git -C /tmp/primer3 rev-parse HEAD)" = 7f9f17d6012f404e83bbf0f931a4d06eb4af465b && test -x /tmp/primer3/src/oligotm && test -x /tmp/primer3/src/ntthal`
	}
	return fmt.Sprintf("\necho '%s  primers.fasta' | sha256sum -c -\n%s\n", markerTrials[trial].outputSHA, packageCheck)
}
