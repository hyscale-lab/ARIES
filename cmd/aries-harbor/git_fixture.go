package main

// Original build-time timestamps determine the object names in later literal
// commands. Reconstruct the three official commits in the newly created guest
// using the image's existing fixture blob. Verify all identities before moving
// the replacement into the task workdir. This runs before the ready snapshot.
const gitRecoveryReady = `set -eu
test ! -e /app/secret.txt
cd /app
test -d repo/.git
test -z "$(git -C repo status --porcelain)"
if test "$(git -C repo rev-parse HEAD)" != e95155fb5729c5dbf7d951cc262be482ecda79c8; then
    stage=$(mktemp -d /tmp/aries-git-setup.XXXXXXXX)
    git -C repo cat-file blob 52d18a091cd9755bacb8eaccab2882a24b080dc7 > "$stage/fixture"
    export GIT_AUTHOR_DATE='1788207377 +0000' GIT_COMMITTER_DATE='1788207377 +0000'
    branch=$(git -C repo symbolic-ref --short HEAD)
    git init -q -b "$branch" "$stage/repo"
    cd "$stage/repo"
    git config user.name TerminalBench
    git config user.email tb@example.com
    printf 'demo project\n' > README.md
    git add README.md
    git commit -qm 'chore: init e665d1a2-04a7-4e61-b523-e897e231d9cc'
    mv "$stage/fixture" secret.txt
    git add secret.txt
    git commit -qm 'feat: add scratch notes'
    test "$(git rev-parse HEAD)" = 6bc24dc08704c86d0a14351265416b8af535a015
    git reset -q --hard HEAD~1
    printf 'some changes\n' >> README.md
    git add README.md
    git commit -qm 'chore: init e665d1a2-04a7-4e61-b523-e897e231d9cc'
    test "$(git rev-parse HEAD)" = e95155fb5729c5dbf7d951cc262be482ecda79c8
    cd /app
    mv repo "$stage/old"
    mv "$stage/repo" repo
    rm -r -- "$stage"
fi
git -C repo rev-parse HEAD
git -C repo fsck --no-reflogs --unreachable | sort`

// The two sources each print one randomly allocated name, then pass it as a
// literal to later calls. Preserve the native allocator's flags, permissions,
// collision handling and descriptor by controlling only its first name.
func gitRecoveryTemp(trial string) string {
	var suffix string
	switch trial {
	case "713938da-5ff4-4af3-b2b8-849e08291aaa":
		suffix = "mvfalbso"
	case "cd728dac-1ca6-4a95-a7c3-16ad40797278":
		suffix = "kbxrtcaa"
	default:
		return ""
	}
	return `
site_dir=$(/usr/bin/python3 -c 'import site; print(site.getsitepackages()[0])')
mkdir -p "$site_dir"
test ! -e "$site_dir/aries_source_temp.py" && test ! -e "$site_dir/aries_source_temp.pth"
cat > "$site_dir/aries_source_temp.py" <<'ARIESTEMP'
import itertools
import sys
import tempfile
original_names = tempfile._get_candidate_names
def source_names():
    caller = sys._getframe(1)
    if caller.f_code in (tempfile._mkstemp_inner.__code__, tempfile.mkdtemp.__code__) and caller.f_locals.get("prefix", caller.f_locals.get("pre")) in ("repo-secret-cleanup-", "repo-secret-audit-"):
        return itertools.chain(("` + suffix + `",), original_names())
    return original_names()
tempfile._get_candidate_names = source_names
ARIESTEMP
echo 'import aries_source_temp' > "$site_dir/aries_source_temp.pth"
python3 -c 'import sys; assert "aries_source_temp" in sys.modules'`
}
