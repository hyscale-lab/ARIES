#!/usr/bin/env bash
set -euo pipefail
task=$1 stage=$2 sandbox=$3
base=/local/zb-study3-stock/local-sol
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
upload() { "$AENV_CLI" upload "$sandbox" "$1" "$2"; }
case "$task" in sam3-inference|sam3-debug);; *) exit 2;; esac
if [[ "$stage" == upload ]]; then
    upload "$base/$task/environment.tar" /tmp/sam-source.tar
    upload "$base/sam3-shared/wheels.tar" /tmp/sam-wheels.tar
    guest sh -ec 'cd /workload/rootfs/tmp_workspace; test ! -e sam3; tar xf /tmp/sam-source.tar; mkdir /workload/rootfs/tmp/sam-wheels; tar xf /tmp/sam-wheels.tar -C /workload/rootfs/tmp/sam-wheels; rm /tmp/sam-source.tar /tmp/sam-wheels.tar'
    for part in "$base"/sam3-shared/weight-[0-9][0-9]; do
        upload "$part" /tmp/sam-weight.part
        guest sh -ec 'cat /tmp/sam-weight.part >>/workload/rootfs/tmp_workspace/sam3/sam3.pt; rm /tmp/sam-weight.part'
    done
    if [[ "$task" == sam3-inference ]]; then upload "$(dirname "$0")/paper-image.mjs" /workload/rootfs/tmp/paper-image.mjs; fi
    exit
fi
[[ "$stage" == ready ]]
guest runc exec zb-workload sh -ec 'cd /tmp/sam-wheels; echo "003f7597d79c828a72e659a8d9668af88bf9b3e7d49ba233bfd482477a46ebc6  SHA256SUMS" | sha256sum -c -; sha256sum -c SHA256SUMS; cd /tmp_workspace; echo "9999e2341ceef5e136daa386eecb55cb414446a00ac2b55eb2dfd2f7c3cf8c9e  sam3/sam3.pt" | sha256sum -c -; test ! -e results; PIP_NO_INDEX=1 PIP_FIND_LINKS=/tmp/sam-wheels bash .wildclaw/run-warmup.sh; rm -r /tmp/sam-wheels; /root/miniconda3/envs/eval/bin/python -c "import numpy,cv2,torch; print(numpy.__version__,cv2.__version__,torch.__version__); assert not torch.cuda.is_available() and numpy.__version__==\"1.26.4\" and cv2.__version__==\"4.9.0\""; nproc; free -b'
