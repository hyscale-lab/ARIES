package main

import "fmt"

// These source-bound Ubuntu sessions operate on the same published fluorescent-tag fixture.
const markerTask = "dna-assembly"

var markerTrials = map[string]struct{ stamp, outputSHA string }{
	"ddf7548f-de77-4e90-aa31-ae1f86311601": {"20260831T212241Z", "6840d29c7c6f5ab1c28b3be5cce0bada7a74cf978a496e6d5a632191922279fc"},
	"8968e176-fa1f-4d6a-8e5a-3ebd7cc0b8de": {"20260831T202034Z", "fee914d1d9a30cc385d25613ae53c0f3777ac79e4bc4ca01aedf66800fe18059"},
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
	return fmt.Sprintf("\necho '%s  primers.fasta' | sha256sum -c -\ntest \"$(dpkg-query -W -f='${Version}' primer3)\" = 2.6.1-4\n", markerTrials[trial].outputSHA)
}
