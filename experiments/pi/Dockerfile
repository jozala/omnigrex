FROM ghcr.io/superradcompany/microsandbox@sha256:be3d9f5f99b937ac4c4452b2ff33657a4ef74a117fdb584abb6d3c7db53e248f AS microsandbox
FROM node:24.14.1-bookworm-slim@sha256:b506e7321f176aae77317f99d67a24b272c1f09f1d10f1761f2773447d8da26c

WORKDIR /opt/probe
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund && npm cache clean --force
COPY --from=microsandbox /usr/local/bin/msb /usr/local/bin/msb
COPY --from=microsandbox /usr/local/lib/ /usr/local/lib/
COPY --from=microsandbox /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY fixtures/ ./fixtures/
RUN mkdir -p /state /workspace /run/pi-agent && chown -R 10001:10001 /state /workspace /run/pi-agent
ENV HOME=/tmp PI_CODING_AGENT_DIR=/run/pi-agent PI_OFFLINE=1 PI_TELEMETRY=0 PI_SKIP_VERSION_CHECK=1
USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/bin/sh", "/opt/probe/fixtures/start.sh"]
