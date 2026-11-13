package bridgekit

import "strings"

// RemoteShellPath is the shell a Hermes tool command runs under, whichever
// transport carries it. The sandbox requires an absolute command path and
// performs no PATH lookup of its own, so the bridge names it here. This is why
// a Hermes task image must provide /bin/bash.
const RemoteShellPath = "/bin/bash"

// ValidWorkdir keeps the sandbox workdir free of characters that would change
// meaning once it becomes a process working directory or appears in evidence.
func ValidWorkdir(value string) bool {
	if value == "/" {
		return true
	}
	if len(value) < 2 || value[0] != '/' || value[len(value)-1] == '/' {
		return false
	}
	for _, component := range strings.Split(value[1:], "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, character := range component {
			if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
				continue
			}
			return false
		}
	}
	return true
}
