package main

const noShellPatch = `if command -v apply_patch; then exit 1; fi`

var walRecoverySetup = taskSetup{
	output: "serializer.py",
	initialCheck: `export LC_ALL=C
test "$(sha256sum *.py | sha256sum)" = '7c5052441d55f801c39bbe1db69720485320b1bcd894073ec53dc6f4c973d6de  -'
test ! -e .git
` + noShellPatch,
}

var sessionWindowSetup = taskSetup{
	output: "app/events.py",
	initialCheck: `export LC_ALL=C
test "$(sha256sum app/*.py app/DESIGN.md | sha256sum)" = 'a5f81707183bc91605ad5cea03b3fc6d36695a171d017fa6833e0a14556cc264  -'
test ! -e .git
test ! -e app/.git
` + noShellPatch,
	inputCheck: `test "$(sha256sum app/__init__.py app/types.py app/DESIGN.md | sha256sum)" = '66679ab004783a3917076102ec553db4163e65da097bdc40063c105c75a9aee2  -'`,
}

var cargoFlightSetup = taskSetup{
	output: "/output/flight_plan.json",
	initialCheck: `export LC_ALL=C
test "$(sha256sum *.py data/*.json | sha256sum)" = '553ff814447a248752ab7278e547c8d5ab4353b87115083f227979512c4fba9a  -'
test -d /output
test ! -e /output/flight_plan.json
test ! -e .git
` + noShellPatch,
	inputCheck: `export LC_ALL=C
test "$(sha256sum data/*.json | sha256sum)" = 'be11bd4dbade41b92b77b7f0be9334b23bb3b46f58d67fc071171da4d0cb6982  -'`,
}

var coqBlockSetup = taskSetup{
	output: "Main.v",
	initialCheck: `echo 'dd3552e0af3b757ad66d9a7c6efb589a54d5010357980e2638fd710d060f6f6e  Main.v' | sha256sum -c -
coqc --version | grep 'version 8.18.0'
coqc --version | grep 'OCaml 4.13.1'
test ! -e Main.vo
` + noShellPatch,
	inputCheck: `echo 'e76ac036d4e920f3917b0a50c40842172e261d85c8b40d4dbbc73a9e3f89ed70  _CoqProject' | sha256sum -c -`,
}

var tb41PatchAliases = map[string]string{
	"a4d84523-40cc-4174-95ba-3b8db9883aee": "QBVFWp",
	"6c8b8d90-1910-46d9-a0a2-18a330d69a71": "tRCUcS",
	"e2cdc89f-e58d-4c66-b906-19bdd0956af2": "7h6rxD",

	"b253c7f7-9591-448e-8e10-c0f35e2771d9": "xolPyo",
	"c782489c-4925-42a5-9873-42eb5dd86a86": "0dBF6j",
	"9fb8d078-0d6a-4629-bcac-1d246656345f": "jRd0KF",
	"3494bc2e-744c-4565-b6d7-90c7894218f1": "ynSWtA",
	"742a1a2c-24e5-41dc-83ab-745b7a50a392": "rziLTy",
}

func tb41SourceSetup(trial string, task taskSetup) taskSetup {
	switch trial {
	case "9fd6a8b8-3075-4054-9caa-3aa13f6bd407", "9b95500e-748c-4b16-914d-eb4ce21c9773", "9f99d667-6eb7-450a-a715-2eb335095856":
		task.packagesSHA = "25a171383f021a29ad1b6028b77ef1a019e8b385e4b673acd763186ef8f5c892"
		task.packagesField = "cad_pillow_wheel_archive_sha256"
		task.initialCheck += `
echo '23d27a3e0307ec2244cc51e7287b919aa68d097504ebe19df4e76a98a3eea5bd  /opt/zb-cad/wheels/pillow-12.3.0-cp311-cp311-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl' | sha256sum -c -
/opt/conda/bin/python -c 'import importlib.util; assert importlib.util.find_spec("PIL") is None'`

	case "23163f44-25fb-47c7-8a9c-5aabc8e918f6", "c1962991-d260-46b5-8047-535da8dd7955",
		"ecd96589-9286-4639-918e-d6a81933aa6f", "42a81732-26cc-4817-8e52-ae846d24410e":
		task.packagesSHA = "e45d5f8a8fcf4e15144f869adeb010553493c791d1b2cec2d7c03b6ce2bb2fbe"
		task.packagesField = "bookworm_git_archive_sha256"
		task.initialCheck += `
test "$(git --version)" = 'git version 2.39.5'
git --version`
	case "3494bc2e-744c-4565-b6d7-90c7894218f1":
		task.packagesSHA = "d0f04134a875e55e61c5441abd559a7d12657ee9a7ba1f7b2e736573ff1ee6fc"
		task.packagesField = "bookworm_git_archive_sha256"
		task.initialCheck += `
test "$(git --version)" = 'git version 2.39.5'
git --version`
	case "8c294d70-dba6-43c3-b041-409880af6418", "75a70fea-d015-48c3-b800-f6db6f2c8bcf",
		"7bb66704-bb91-48e2-833d-22d22fd88e91", "55c43499-8c78-45d5-8b18-9b94ecf65ec2":
		task.packagesSHA = "b5bde9f81915e55cd43ab0c2f0236ef2de9141701d8a369cb8320fd7c176d5b1"
		task.packagesField = "trixie_git_archive_sha256"
		task.initialCheck += `
test "$(git --version)" = 'git version 2.47.3'
git --version`
	case "bc842776-b412-4c72-8cc4-508c45838176", "e06d4044-711f-464c-819a-167164b8f3a2":
		task.initialCheck += "\n" + sourceHarnessDirectory
	}
	suffix := tb41PatchAliases[trial]
	if suffix == "" {
		suffix = bipedPatchAliases[trial]
	}
	if suffix != "" {
		task.initialCheck += "\nalias_dir=/root/.codex/tmp/arg0/codex-arg0" + suffix + `
target=/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
test -x "$target"
mkdir -p "$alias_dir"
ln -s "$target" "$alias_dir/apply_patch"
readlink "$alias_dir/apply_patch"`
	}
	return task
}

const cadReady = `/opt/conda/bin/python -c 'import FreeCAD,sys; assert FreeCAD.Version()[:3]==["0","21","2"] and sys.version_info[:2]==(3,11)'
` + noShellPatch
