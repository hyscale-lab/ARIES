"""Render the measured stack provisioner for a fixed CPU training OCI image."""
import argparse
import hashlib
from pathlib import Path


def render(task):
    names = {'vpp-loss-divergence': 15360, 'pretrain-shard-corruption': 4096}
    source = (Path(__file__).resolve().parents[1] / 'provision_baseline.sh').read_bytes()
    if hashlib.sha256(source).hexdigest() != '5d33d8e7840520046f1b94a1ca4eb323e350ec1babd3392000a2be81cf591371':
        raise ValueError('verify changed baseline against the study runtime before use')
    text = source.decode().replace('10737418240', str(names[task] * 2**20))
    old = '"${ctr[@]}" images pull --local --platform linux/amd64 --snapshotter devmapper "$image"'
    new = '''archive=$(realpath "$6")
tag=$7
test -s "$archive"
test "$tag" = docker.io/library/zb-tb41-TASK:qualified
"${ctr[@]}" images import --local --platform linux/amd64 --snapshotter devmapper --digests - < "$archive"
"${ctr[@]}" images tag "$tag" "$image"
"${ctr[@]}" images ls | awk -v expected="$image" '$1 == expected && $3 == substr(expected,index(expected,"@")+1) { matched=1 } END { exit !matched }' '''.rstrip()
    if text.count(old) != 1:
        raise ValueError('unexpected image provisioning path')
    return text.replace(old, new.replace('TASK', task))


if __name__ == '__main__':
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument('task', choices=['vpp-loss-divergence','pretrain-shard-corruption'])
    print(render(cli.parse_args().task), end='')
