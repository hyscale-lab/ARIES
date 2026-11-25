"""Render the two fixed CPU training image recipes for the OCI builder."""
import argparse
import base64
import hashlib
from pathlib import Path


def render(source):
    digest = hashlib.sha256(source).hexdigest()
    text = source.decode().replace('FROM python:', 'FROM docker.io/library/python:', 1)
    if digest == '2d00823b3f8e4bfdccc3c32f0d81ee98ac9092cc2926f8302a2f766cfe2cdf1b':
        return text
    if digest != 'b15c41b25598569bc90891b52c54166e72b109c1cdd147b64724ba39a7c1355e':
        raise ValueError('unrecognized source Dockerfile')
    # Older Podman parses Python imports inside Docker heredocs as instructions.
    before, rest = text.split("RUN python - <<'PYEOF'\n", 1)
    code, after = rest.split('\nPYEOF\n', 1)
    encoded = base64.b64encode((code+'\n').encode()).decode()
    text = before+'RUN echo '+encoded+' | base64 -d | python -\n'+after
    # The timestamp-unbounded resolver otherwise replaces the source CPU wheel.
    anchor = '        "lightning==2.5.0"'
    return text.replace(anchor, '        "torch==2.6.0+cpu" '+chr(92)+'\n'+anchor, 1)


if __name__ == '__main__':
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument('source', type=Path)
    args = cli.parse_args()
    print(render(args.source.read_bytes()), end='')
