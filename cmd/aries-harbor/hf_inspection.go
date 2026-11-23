package main

import (
	"context"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

const (
	hfInspectionTrial    = "071ce48d-3de4-4c02-9e42-3eb64933acd2"
	hfPluginsCommit      = "1e285826e604f66f7208f7ac4dba0fe8341d1f57"
	hfPluginsArchive     = "6482d01621431880a077fb4305b245bc6a7909a8e8f875cc4ea06c7c6f268d4e"
	hfInspectionPackages = "36ae1b315f30cab6e9b06c4e81d7cb8e512658a2b799108b9080ff1b6999a74c"
)

// The recorded tools enumerate the checkout using find and rg. Its contents
// stay in the workload guest as data, outside the replay driver's environment.
func prepareHFInspection(ctx context.Context, m *vhive.Manager, id, directory string) error {
	if err := installPackages(ctx, m, id, filepath.Join(directory, "hf-tools.tar.gz"), hfInspectionPackages); err != nil {
		return err
	}
	const destination = "/tmp/codex-home/.tmp"
	if _, err := checked(ctx, m, id, core.Command{Path: "/bin/mkdir", Args: []string{"-p", destination}}); err != nil {
		return err
	}
	if err := uploadPinned(ctx, m, id, filepath.Join(directory, "upstream.tar.gz"), hfPluginsArchive, destination); err != nil {
		return err
	}
	_, err := checked(ctx, m, id, core.Command{Path: "/bin/sh", Dir: destination, Args: []string{"-ec",
		"test ! -e plugins && mv plugins-" + hfPluginsCommit + " plugins && printf '%s\\n' " + hfPluginsCommit + " > plugins.sha"}})
	return err
}

const hfInspectionReady = `
test "$(command -v git)" = /usr/bin/git
test "$(command -v patch)" = /usr/bin/patch
test "$(command -v curl)" = /usr/bin/curl
git --version
patch --version | head -1
curl --version | head -1
test "$(cat /tmp/codex-home/.tmp/plugins.sha)" = ` + hfPluginsCommit + `
(cd /tmp/codex-home/.tmp/plugins && sha256sum -c <<'DOCS'
6327efcce0c631580cee5bf2dcac8f884a81172a0261fa3c8ca458a72f603165  plugins/build-web-apps/skills/react-best-practices/AGENTS.md
bc26c551fda72e50b66e132854aa63ead7280c93b0d1f6bbb6c4f841e7c1202e  plugins/build-web-apps/skills/supabase-best-practices/AGENTS.md
fa92c49b2e5d991383724f4cc1ebf4960840c608603b895602ee60175feb7282  plugins/data-analytics/AGENTS.md
a2790a23f9c65debc9e377512b74635b44c6403c3b4ef6eb72e1a6055ff4cd15  plugins/product-design/templates/mobile-app/AGENTS.md
4e20d00d0612fe53f04c26b11175879780ae6ba4a4d2245af9599f50e448e505  plugins/product-design/templates/prototype/AGENTS.md
722aa11cb37a6fc3748414c095870e8547b95b370152371272ca2afb8db880f4  plugins/vercel/skills/react-best-practices/AGENTS.md
41d8bbb7a5d4256c4d31a20844a204df79ab7712d502c90c5f3f8065a838bb6b  plugins/zoom/AGENTS.md
DOCS
)
`
