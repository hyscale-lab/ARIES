package bridge

import (
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
)

// Revision is the commit the aries binary was built from, set by `make build`
// and k8s/create-cluster.sh with
// -ldflags "-X github.com/hyscale-lab/aries/internal/app/wiring/bridge.Revision=<commit>",
// and suffixed -dirty when tracked files had uncommitted changes. A plain
// `go build` from a checkout leaves it empty; Go's own build stamp is used then.
var Revision string

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{12}(-dirty)?$`)

// KubernetesImage is the bridge image a Kubernetes run requests: the pinned
// image with the runner's commit appended to its tag, such as
// aries-bridge:v1-0123456789ab. A cluster pulls the bridge by name and a node
// keeps whatever image it already holds under that name, so a fixed tag would
// keep serving an old bridge after the code changed. With the commit in the
// tag, a bridge that was not published for this code cannot be pulled, and the
// run fails visibly instead. k8s/publish-images.sh pushes under the same tag.
func KubernetesImage(pinned string) (string, error) {
	commit := Revision
	if commit == "" {
		commit = buildRevision(debug.ReadBuildInfo)
	}
	if commit == "" {
		return "", errors.New("the Kubernetes bridge image is tagged with the commit ARIES was built from, and this binary records none; build it with `make build` from a git checkout")
	}
	if !revisionPattern.MatchString(commit) {
		return "", fmt.Errorf("build revision %q is not a 12-character commit", commit)
	}
	return pinned + "-" + commit, nil
}

// buildRevision reads the commit Go stamps into a binary built from a git
// checkout, shortened to twelve characters.
func buildRevision(read func() (*debug.BuildInfo, bool)) string {
	info, ok := read()
	if !ok {
		return ""
	}
	var commit string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			commit = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if len(commit) < 12 {
		return ""
	}
	commit = commit[:12]
	if modified {
		commit += "-dirty"
	}
	return commit
}
