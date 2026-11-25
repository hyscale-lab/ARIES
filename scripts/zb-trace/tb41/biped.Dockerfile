FROM docker.io/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a

ENV DEBIAN_FRONTEND=noninteractive
ENV PIP_DISABLE_PIP_VERSION_CHECK=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip python3-venv ca-certificates libx11-6 libsm6 libglib2.0-0t64 git patch \
    && dpkg-query -W > /usr/local/share/zb-biped-packages.tsv \
    && rm -rf /var/lib/apt/lists/*

RUN --mount=type=bind,source=wheelhouse,target=/wheelhouse,readonly \
    python3 -m venv /opt/biped-env \
    && /opt/biped-env/bin/python -m pip install --no-index --no-deps --require-hashes --find-links=/wheelhouse -r /wheelhouse/requirements.txt \
    && /opt/biped-env/bin/python -m pip check
ENV PATH="/opt/biped-env/bin:${PATH}"
RUN printf '%s\n' 'export PATH=/opt/biped-env/bin:$PATH' > /etc/profile.d/zz-biped-python.sh
WORKDIR /app
COPY data/ /app/
RUN mkdir -p /app/results /app/submission
