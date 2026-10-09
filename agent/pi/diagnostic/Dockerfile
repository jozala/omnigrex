FROM node:24.14.1-bookworm-slim@sha256:b506e7321f176aae77317f99d67a24b272c1f09f1d10f1761f2773447d8da26c
WORKDIR /opt/omnigrex/pi
COPY experiments/pi/package.json experiments/pi/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund && npm cache clean --force
COPY agent/pi/diagnostic/fixture.mjs ./fixture.mjs
RUN printf '#!/bin/sh\nexec /usr/local/bin/node /opt/omnigrex/pi/fixture.mjs\n' > /usr/local/bin/omnigrex-pi-diagnostic && chmod 0755 /usr/local/bin/omnigrex-pi-diagnostic && \
    mkdir -p /workspace /run/pi-agent && chown 10001:10001 /workspace /run/pi-agent && \
    test "$(node --version)" = v24.14.1 && test "$(node node_modules/@earendil-works/pi-coding-agent/dist/cli.js --version)" = 1.0.4
ENV HOME=/tmp PI_CODING_AGENT_DIR=/run/pi-agent PI_OFFLINE=1 PI_TELEMETRY=0 PI_SKIP_VERSION_CHECK=1
USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/omnigrex-pi-diagnostic"]
