# Use the reconstructed original application image as APP_IMAGE.
ARG APP_IMAGE
FROM ${APP_IMAGE} AS dated
# Bootstrap apt TLS from Node's CA bundle. Apt still verifies Debian signatures.
RUN node -p 'require("node:tls").rootCertificates.join("\n")' > /tmp/aries-ca.pem \
    && sed -i -e 's|http://deb.debian.org/debian-security|https://snapshot.debian.org/archive/debian-security/20260831T100834Z/|' \
        -e 's|http://deb.debian.org/debian|https://snapshot.debian.org/archive/debian/20260831T100834Z/|' \
        -e '/^Signed-By:/a Check-Valid-Until: no' /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::https::CaInfo=/tmp/aries-ca.pem update -qq -o APT::Update::Error-Mode=any \
    && apt-get -o Acquire::https::CaInfo=/tmp/aries-ca.pem install -y --no-install-recommends ca-certificates curl \
    && rm -f /tmp/aries-ca.pem \
    && rm -rf /var/lib/apt/lists/*
ENV npm_config_before=2026-08-31T10:08:34Z
FROM dated AS helper
RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends python3 patchelf \
    && mkdir -p /opt/aries-python/bin /opt/aries-python/lib \
    && cp /usr/bin/python3.11 /opt/aries-python/bin/python3 \
    && cp -a /usr/lib/python3.11 /opt/aries-python/lib/
RUN cp -L /usr/lib/x86_64-linux-gnu/libexpat.so.1 /usr/lib/x86_64-linux-gnu/libffi.so.8 /opt/aries-python/lib \
    && patchelf --set-rpath '$ORIGIN/../lib' /opt/aries-python/bin/python3 \
    && patchelf --set-rpath '$ORIGIN/../..' /opt/aries-python/lib/python3.11/lib-dynload/_ctypes.*.so
FROM dated
COPY --from=helper /opt/aries-python /opt/aries-python
ENV WAREHOUSE_API_URL=http://127.0.0.1:4100 AUDIT_URL=http://127.0.0.1:4101
RUN ! command -v python3 \
    && /opt/aries-python/bin/python3 -I -S -c "import ctypes,os,pathlib,selectors,signal,socket,threading; assert os.path.isdir('/opt/aries-python/lib/python3.11')"
