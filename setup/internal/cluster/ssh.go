package cluster

import (
	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

// sshClient shells out to the operator's own ssh, so ~/.ssh/config, agents and
// jump hosts all behave exactly as they do interactively.
type sshClient struct {
	options []string
}

func newSSH(cluster configs.Cluster) sshClient {
	options := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	options = append(options, cluster.SSHOptions...)
	if cluster.SSHKey != "" {
		options = append(options, "-i", cluster.SSHKey)
	}
	return sshClient{options: options}
}

// Line renders the local shell line that runs remoteCmd on target. -n detaches
// stdin: ssh forwards it by default, and a remote call inside a loop would
// otherwise consume the loop's own input and hang.
//
// remoteCmd is quoted as one word, so the local shell hands it to ssh intact
// and the remote shell is the only one that parses it.
func (s sshClient) Line(target, remoteCmd string) string {
	return "ssh -n " + utils.QuoteAll(s.options...) + " " + utils.Quote(target) + " " + utils.Quote(remoteCmd)
}

func (s sshClient) run(target, remoteCmd string) (string, error) {
	return utils.ExecShellCmd(s.Line(target, remoteCmd))
}

func (s sshClient) runSecret(target, remoteCmd string) (string, error) {
	return utils.ExecShellCmdSecret(s.Line(target, remoteCmd))
}

func (s sshClient) stream(target, remoteCmd string) error {
	return utils.ExecShellCmdStreaming(s.Line(target, remoteCmd))
}

// stage replaces the node's staged directory with the contents of dir. tar
// over ssh needs no destination to exist first and keeps the exec bit.
func (s sshClient) stage(target, dir string) error {
	_, err := utils.ExecShellCmd(s.StageLine(target, dir))
	return err
}

// StageLine renders the tar-over-ssh pipeline for stage. This is the one ssh
// call without -n, because its stdin is the archive.
//
// COPYFILE_DISABLE and the exclusions are what keep a macOS operator's
// filesystem metadata out of the archive. Files on macOS routinely carry
// extended attributes — curl alone tags a download with com.apple.provenance —
// and bsdtar serialises each one as a companion AppleDouble member named
// "._<file>". Those members are binary. Landing them in a Helm chart is not
// cosmetic: helm reads every file in the chart's crds/ directory, and "._"
// sorts before any letter, so the first thing it parses is a resource fork and
// the install dies on "control characters are not allowed". COPYFILE_DISABLE=1
// stops bsdtar generating them; the excludes also drop any that are already on
// disk, and are accepted by both bsdtar and GNU tar.
func (s sshClient) StageLine(target, dir string) string {
	remote := "rm -rf " + remoteDir + " && mkdir -p " + remoteDir + " && tar -C " + remoteDir + " -xf -"
	return "COPYFILE_DISABLE=1 tar -C " + utils.Quote(dir) +
		" --exclude " + utils.Quote("._*") + " --exclude " + utils.Quote(".DS_Store") +
		" -cf - . | ssh " +
		utils.QuoteAll(s.options...) + " " + utils.Quote(target) + " " + utils.Quote(remote)
}
