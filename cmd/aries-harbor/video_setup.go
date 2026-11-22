package main

// Original video trial 020f911b, step 7, reports OpenCV 5.0.0, NumPy
// 2.5.2, Python 3.13, and opencv-contrib-python. The published OCI image
// contains OpenCV 4.12.0/NumPy 2.2.6; it renders different text into frames.
// Preserve PyPI's exact compatible wheel hashes in the pre-snapshot receipt.
const videoInitialCheck = `test ! -e output.toml
test ! -e jump_analyzer.py
python -c 'import cv2,numpy; print("published image",cv2.__version__,numpy.__version__)'
cat > /tmp/aries-video-requirements.txt <<'WHEELS'
numpy @ https://files.pythonhosted.org/packages/7b/44/59a1eb68e773c4098d107ef34a0dbdeca501d72ffcfbff9a7707343921ce/numpy-2.5.2-cp313-cp313-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl --hash=sha256:29b86ff8a6cc556b47ec6b64b194815cc80e6bf5eedcc6cddfd65318cb0b4eee
opencv-contrib-python @ https://files.pythonhosted.org/packages/8b/39/f87d154d2dca8e9815ca9e4f925aee41e5931162da730d17f3200e7786f3/opencv_contrib_python-5.0.0.93-cp37-abi3-manylinux_2_28_x86_64.whl --hash=sha256:8427dcb0561dc3ba32f3771c627f52b87c29932c265bea28ffcb54804c3c3fec
WHEELS
python -m pip install --disable-pip-version-check --no-cache-dir --no-index --no-deps --require-hashes --progress-bar off -r /tmp/aries-video-requirements.txt
cat /tmp/aries-video-requirements.txt
rm /tmp/aries-video-requirements.txt
python -c 'import cv2,numpy; print("ready guest",cv2.__version__,numpy.__version__); assert (cv2.__version__,numpy.__version__)==("5.0.0","2.5.2")'
printf '%s\n' 'export PIP_REPORT=/tmp/aries-reviewed-pip-install.json' > /etc/profile.d/zz-aries-pip.sh`

// Source step 12 reads this exact symlink and step 18 invokes its patch-only
// argv[0] alias. This trial uses tomllib from Python's standard library.
const videoPatchTrial = "020f911b-467e-49ad-8113-bb725bee1d4b"
const videoPatchAlias = `alias_dir=/root/.codex/tmp/arg0/codex-arg0BTXgJr
target=/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
test -x "$target"
mkdir -p "$alias_dir"
ln -s "$target" "$alias_dir/apply_patch"
readlink "$alias_dir/apply_patch"`

// Source steps 20 and 21 list the harness worktree and locate GNU patch.
// Only the observed directory structure is needed in this image. The native
// validation receipt retains listing differences from the live harness.
const videoHarnessTrial = "07e979df-d2c5-4b88-9638-abcc492c3dc3"
const videoHarnessDirectory = `install -d -m 0755 /tmp/codex-home/.tmp
test -d /tmp/codex-home/.tmp
printf '%s\n' 'prepared /tmp/codex-home/.tmp; original transient harness contents unavailable'`

// Source 80f95afc invokes the GNU patch command when writing its analyzer.
const videoGNUPatchTrial = "80f95afc-7eb6-4fa4-9c39-13abb473cdc8"
