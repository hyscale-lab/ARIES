package main

import (
	"errors"
	"fmt"
	"regexp"
)

// bridgeRevision is the last commit that changed the bridge, as
// scripts/bridge-revision.sh prints it, set by `make build` and
// k8s/create-cluster.sh with -ldflags "-X main.bridgeRevision=<commit>". It
// ends in -dirty when the bridge's files had uncommitted changes. It lives
// here, outside every package the bridge compiles, so changing it does not
// itself count as a bridge change.
var bridgeRevision string

var bridgeRevisionPattern = regexp.MustCompile(`^[0-9a-f]{12}(-dirty)?$`)

// kubernetesBridgeImage is the bridge image a Kubernetes run requests: the
// pinned image with the bridge's last commit appended to its tag, such as
// aries-bridge:v1-0123456789ab. A cluster pulls the bridge by name and a node
// keeps whatever image it already holds under that name, so a fixed tag would
// keep serving an old bridge after the code changed. With the commit in the
// tag, a bridge that was not published for this code cannot be pulled, and the
// run fails visibly instead; commits that leave the bridge alone keep the tag.
// k8s/publish-images.sh pushes under the same tag.
func kubernetesBridgeImage(pinned string) (string, error) {
	if bridgeRevision == "" {
		return "", errors.New("the Kubernetes bridge image is tagged with the commit that last changed the bridge, and this binary records none; build it with `make build` from a git checkout")
	}
	if !bridgeRevisionPattern.MatchString(bridgeRevision) {
		return "", fmt.Errorf("bridge revision %q is not a 12-character commit", bridgeRevision)
	}
	return pinned + "-" + bridgeRevision, nil
}
