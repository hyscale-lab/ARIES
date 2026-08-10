package openclaw

import "embed"

const (
	e2bPluginContainerDir = "/opt/aries/openclaw/aries-e2b"
	e2bTokenContainerPath = "/run/aries/e2b/access.token"
)

//go:embed assets/aries-e2b/*
var e2bPluginAssets embed.FS

func stagedE2BPluginFiles() (map[string]stagedFile, error) {
	files := make(map[string]stagedFile)
	entries, err := e2bPluginAssets.ReadDir("assets/aries-e2b")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		content, err := e2bPluginAssets.ReadFile("assets/aries-e2b/" + entry.Name())
		if err != nil {
			return nil, err
		}
		mode := int64(0o444)
		if entry.Name() == "helper.mjs" {
			mode = 0o555
		}
		files["opt/aries/openclaw/aries-e2b/"+entry.Name()] = stagedFile{content: content, mode: mode}
	}
	return files, nil
}
