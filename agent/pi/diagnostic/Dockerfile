FROM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS fixture-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY agent/pi ./agent/pi
RUN CGO_ENABLED=0 go build -o /out/pi-mcp-fixture ./internal/runtime/pi/fixture

FROM fixture-build AS search-tools
ARG TARGETARCH
RUN case "$TARGETARCH" in \
      amd64) \
        rg_target=x86_64-unknown-linux-musl; rg_sha=1c9297be4a084eea7ecaedf93eb03d058d6faae29bbc57ecdaf5063921491599; \
        fd_target=x86_64-unknown-linux-musl; fd_sha=2b6bfaae8c48f12050813c2ffe1884c61ea26e750d803df9c9114550a314cd14 ;; \
      arm64) \
        rg_target=aarch64-unknown-linux-gnu; rg_sha=2b661c6ef508e902f388e9098d9c4c5aca72c87b55922d94abdba830b4dc885e; \
        fd_target=aarch64-unknown-linux-musl; fd_sha=996b9b1366433b211cb3bbedba91c9dbce2431842144d925428ead0adf32020b ;; \
      *) exit 1 ;; \
    esac && \
    mkdir -p /out/search/rg /out/search/fd && \
    wget -q "https://github.com/BurntSushi/ripgrep/releases/download/15.1.0/ripgrep-15.1.0-${rg_target}.tar.gz" -O /tmp/rg.tar.gz && \
    echo "$rg_sha  /tmp/rg.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/rg.tar.gz -C /out/search/rg --strip-components=1 && \
    wget -q "https://github.com/sharkdp/fd/releases/download/v10.3.0/fd-v10.3.0-${fd_target}.tar.gz" -O /tmp/fd.tar.gz && \
    echo "$fd_sha  /tmp/fd.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/fd.tar.gz -C /out/search/fd --strip-components=1

FROM node:24.14.1-bookworm-slim@sha256:b506e7321f176aae77317f99d67a24b272c1f09f1d10f1761f2773447d8da26c
WORKDIR /opt/omnigrex/pi
COPY agent/pi/package.json agent/pi/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund && npm cache clean --force
COPY agent/pi/diagnostic/fixture.mjs ./fixture.mjs
COPY agent/pi/runner.mjs agent/pi/mcp-extension.mjs agent/pi/failure-extension.mjs agent/pi/session-state.mjs agent/pi/session-validation.mjs agent/pi/history-rpc.mjs ./
COPY agent/pi/subscription-auth.mjs agent/pi/step-budget.mjs ./
COPY agent/pi/diagnostic/configuration.mjs ./configuration.mjs
COPY agent/pi/diagnostic/seed-session.mjs ./seed-session.mjs
COPY agent/pi/diagnostic/mcp-boundary.test.mjs ./mcp-boundary.test.mjs
COPY agent/pi/diagnostic/failure-boundary.test.mjs ./failure-boundary.test.mjs
COPY agent/pi/diagnostic/subscription.test.mjs ./diagnostic/subscription.test.mjs
COPY agent/pi/diagnostic/step-budget.test.mjs ./step-budget.test.mjs
COPY agent/pi/diagnostic/session-state.test.mjs ./session-state.test.mjs
COPY --from=fixture-build /out/pi-mcp-fixture /usr/local/bin/pi-mcp-fixture
COPY --from=search-tools /out/search/rg/rg /out/search/fd/fd /usr/local/bin/
RUN printf '#!/bin/sh\nexec /usr/local/bin/node /opt/omnigrex/pi/fixture.mjs\n' > /usr/local/bin/omnigrex-pi-diagnostic && chmod 0755 /usr/local/bin/omnigrex-pi-diagnostic && \
    mkdir -p /workspace /run/pi-agent && chown 10001:10001 /workspace /run/pi-agent && \
    test "$(node --version)" = v24.14.1 && test "$(node node_modules/@earendil-works/pi-coding-agent/dist/cli.js --version)" = 1.0.4 && \
    rg --version && test "$(fd --version)" = 'fd 10.3.0'
ENV HOME=/tmp PI_CODING_AGENT_DIR=/run/pi-agent PI_OFFLINE=1 PI_TELEMETRY=0 PI_SKIP_VERSION_CHECK=1
USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/omnigrex-pi-diagnostic"]
