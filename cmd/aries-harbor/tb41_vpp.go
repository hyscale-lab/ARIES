package main

const vppLossTask = "vpp-loss-divergence"

var vppLossSetup = taskSetup{
	output: "output/loss_trace.pt",
	inputCheck: `sha256sum -c <<'ARIESVPP'
0f07afa3ce9ac319c278f2238c48496486181509fd8df4b82f954810524c6605  pretrain.py
1e01fb4f5520553995935eecdfa84d5d38fb7628a9487d4ac63c220f26911dc7  config.py
2c612d4e0f37ddb075affd09be49a58c4faf9a33629cbfed9310b2b1414d21dc  /usr/local/lib/python3.11/site-packages/nemo/lightning/megatron_parallel.py
ARIESVPP`,
	initialCheck: `test -d output
test -z "$(ls -A output)"
test "$(command -v patch)" = /usr/bin/patch
command -v torchrun
dpkg-query -W git patch
df -P -B1 .
python - <<'PYVPP'
import sys
from importlib.metadata import version

assert sys.version_info[:2] == (3, 11)
print(sys.version.split()[0])
for name, expected in {"torch": "2.6.0+cpu", "nemo-toolkit": "2.2.1", "megatron-core": "0.12.0", "lightning": "2.5.0", "pytorch-lightning": "2.5.0", "numpy": "1.26.4"}.items():
    actual = version(name)
    assert actual == expected, (name, actual)
    print(name, actual)
PYVPP
` + noShellPatch,
}

const vppLossFinal = `
for name in evaluation_loop prediction_loop; do
    sha256sum "/usr/local/lib/python3.11/site-packages/lightning/pytorch/loops/$name.py"
done`
