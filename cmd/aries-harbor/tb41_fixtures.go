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
	"bd4fd537-e83d-4864-a71d-d6e4911288e4": "BYIvm9",
	"a2334355-6f34-41d5-9499-dc4873806dfb": "xtwPGq",
	"a4d84523-40cc-4174-95ba-3b8db9883aee": "QBVFWp",
	"6c8b8d90-1910-46d9-a0a2-18a330d69a71": "tRCUcS",
	"e2cdc89f-e58d-4c66-b906-19bdd0956af2": "7h6rxD",

	"b253c7f7-9591-448e-8e10-c0f35e2771d9": "xolPyo",
	"c782489c-4925-42a5-9873-42eb5dd86a86": "0dBF6j",
	"9fb8d078-0d6a-4629-bcac-1d246656345f": "jRd0KF",
	"3494bc2e-744c-4565-b6d7-90c7894218f1": "ynSWtA",
	"742a1a2c-24e5-41dc-83ab-745b7a50a392": "rziLTy",
	"6d75b013-821a-49ba-b20e-97d3b5c06633": "6zirgA",
	pretrainShardLiveTrial:                 "zWAhk1",

	"16f06a85-5b3e-43db-babc-8d2cf1d31497": "hNolZh",
	"0829d780-a19e-4717-90d5-1664514bb797": "eTxIsb",
	"2b3984d8-7648-4dfb-bd50-2a99957679c9": "FKbvCi",
	"e42bf0ae-155f-48f2-a656-40a3dcc3870c": "tziohC",
}

func tb41SourceSetup(trial string, task taskSetup) taskSetup {
	switch trial {
	case pretrainShardLiveTrial:
		task.initialCheck += `
install -d -m 0700 /tmp/codex-home
test -z "$(ls -A /tmp/codex-home)"
command -v curl
test ! -e repair_training_data.py
test ! -e /tmp/pretrain-investigation`
	case "1d577700-1aa3-4bf8-a92e-d0bb6dea1925":
		task.initialCheck += `
install -d -m 0700 /tmp/codex-home
test -z "$(ls -A /tmp/codex-home)"
test -x /root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
test ! -e /tmp/training_before_repair
test ! -e /tmp/protected-training-files.sha256
test ! -e repair_training_data.py
test ! -e data/train.sha256.json
test ! -e pretrain_recovered.log`
	case "a27cb98a-fce4-4f32-b6b3-7f923ae19176":
		task.initialCheck += "\nmkdir -p /tmp/codex-home/skills\ntest -z \"$(ls -A /tmp/codex-home/skills)\""
	case "b763a908-c85a-451f-94fc-3727a75d1ef0":
		task.packagesSHA = "9f255a2ec972389b541e0ecfb2dc4a2e5597b2aa53899dcedab3e6214aa43a49"
		task.packagesField = "music_fonts_wheels_archive_sha256"
		task.initialCheck += "\ntest -d /usr/share/fonts/truetype/dejavu\ndpkg-query -W fonts-dejavu-core fonts-dejavu-mono"

	case "9b95500e-748c-4b16-914d-eb4ce21c9773":
		task.packagesSHA = "25a171383f021a29ad1b6028b77ef1a019e8b385e4b673acd763186ef8f5c892"
		task.packagesField = "cad_pillow_wheel_archive_sha256"
		task.initialCheck += cadPillowReady
	case "9fd6a8b8-3075-4054-9caa-3aa13f6bd407", "9f99d667-6eb7-450a-a715-2eb335095856":
		task.packagesSHA = "f2624f427abe6c6509eb55c4cedb3fe6cebbf03980eac44be71f5e19af8e4d49"
		task.packagesField = "cad_python_pillow_archive_sha256"
		task.initialCheck += cadPythonReady + cadPillowReady
	case "08255123-7f37-4ac3-ad5a-ec7e9a4e3d39":
		task.packagesSHA = "3534cff3001530a11062e5fe27e2576b3ac19102d2cd449b26ea96f05a44d663"
		task.packagesField = "cad_libgl_python_patch_archive_sha256"
		task.initialCheck += cadPythonReady + cadQtReady + cadPatchReady
	case "ce6044db-e5ea-430d-91e0-7272a0695992":
		task.packagesSHA = "0147cfc122120b0891434f43c9a81db9655cf7f4914d7704789116e54c953ba9"
		task.packagesField = "cad_libgl_python_archive_sha256"
		task.initialCheck += cadPythonReady + cadQtReady
	case "a9e95de2-9628-4159-9e65-d04c4e03c787":
		task.packagesSHA = "072838b30a5952f567a4e68daba091db48872a9908a091de8286f6a8439a1871"
		task.packagesField = "cad_libgl_patch_archive_sha256"
		task.initialCheck += cadQtReady + cadPatchReady
	case "a4d84523-40cc-4174-95ba-3b8db9883aee", "06c8cac1-50a8-4b51-933f-f221f143d7a2":
		task.packagesSHA = "b99b22d5b808b667b8dad5cf03a47d99100844fd17cdd737ada9264704f99990"
		task.packagesField = "cad_libgl_archive_sha256"
		task.initialCheck += cadQtReady
	case "6c8b8d90-1910-46d9-a0a2-18a330d69a71":
		// The recorded directory listing included private harness state.
		task.initialCheck += "\nmkdir /tmp/codex-home\ntest -z \"$(ls -A /tmp/codex-home)\""

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

const cadPatchReady = `
test "$(command -v patch)" = /usr/bin/patch
test "$(dpkg-query -W -f='${Version}' patch)" = 2.7.6-7
patch --version | head -1`

const cadPythonReady = `
/usr/bin/python3 -c 'import importlib.util,sys; assert sys.version_info[:2]==(3,11) and importlib.util.find_spec("PIL") is None; print(sys.version)'`

const cadQtReady = `
/opt/conda/bin/python -c 'from PySide2.QtGui import QImage; assert not QImage(1,1,QImage.Format_RGB32).isNull()'`

const cadPillowReady = `
echo '23d27a3e0307ec2244cc51e7287b919aa68d097504ebe19df4e76a98a3eea5bd  /opt/zb-cad/wheels/pillow-12.3.0-cp311-cp311-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl' | sha256sum -c -
/opt/conda/bin/python -c 'import importlib.util; assert importlib.util.find_spec("PIL") is None'`
