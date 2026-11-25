package main

const bipedTask = "biped-contact-dynamics"

const bipedInputCheck = `sha256sum -c <<'ARIESBIPEDS'
ed386352b430c002834a14d0f7934a9b07e7815163234d251225512bf4aac53e  biped_model.py
c619bad0855160e8ce24ec1846d8bf909a4a12ac42c266a026a3b0d209a1d261  README_2DBiped.md
28b046e84e8404893de2fef9fdbbfe916458268314aa5be712cf794598bd1f46  make_trajectories.py
2df1c9310056b726bae3dc6b6d6b9180f11616d9f7a8ec69c2ad9f38d79d0116  planar_walker.urdf
2d6ec41f21a6ac677417025d56fc645473d003afe61724c1b8d060580bdee42e  configs/visible.json
ARIESBIPEDS`

var bipedSetup = taskSetup{
	output:     "submission/solve.py",
	inputCheck: bipedInputCheck,
	initialCheck: `echo 'b2ac13b22dc129bee6b8686558f0cd3c37916fe2e040322ada92c5a8baee0c09  submission/solve.py' | sha256sum -c -
test ! -e .git
test ! -e /usr/bin/time
test -d results
test -z "$(ls -A results)"
test "$(command -v python)" = /opt/biped-env/bin/python
test "$(command -v git)" = /usr/bin/git
test "$(command -v patch)" = /usr/bin/patch
dpkg-query -W git patch python3
python - <<'PYBIPED'
import sys
from importlib.metadata import version
from biped_model import BipedModel

assert sys.version_info[:2] == (3, 12)
for name, expected in {"pip": "26.1.1", "drake": "1.53.0", "numpy": "2.3.5", "scipy": "1.17.1"}.items():
    actual = version(name)
    assert actual == expected, (name, actual)
    print(name, actual)
plant = BipedModel("planar_walker.urdf").plant
assert (plant.num_positions(), plant.num_velocities(), plant.num_actuators()) == (7, 7, 4)
PYBIPED
` + noShellPatch,
}

var bipedPatchAliases = map[string]string{
	"93de9f31-5128-4723-9c39-e9dbeae1433c": "A45eaV",
	"87dc16e1-1931-4155-9397-33981c437725": "bnvIkp",
}

const bipedFinalCheck = `
for f in results/walk.npz results/jump.npz results/run.npz; do
    if [ -s "$f" ]; then sha256sum "$f"; else printf 'absent %s\n' "$f"; fi
done`
