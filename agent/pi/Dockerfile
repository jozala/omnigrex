FROM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS bootstrap
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/omnigrex-environment ./cmd/omnigrex-environment
COPY internal/runtime/environment ./internal/runtime/environment
COPY internal/turnconfig ./internal/turnconfig
COPY agent/pi ./agent/pi
RUN CGO_ENABLED=0 go build -trimpath -o /out/omnigrex-environment ./cmd/omnigrex-environment

FROM node:24.14.1-bookworm@sha256:80fc934952c8f1b2b4d39907af7211f8a9fff1a4c2cf673fb49099292c251cec AS golden
ARG TARGETARCH
WORKDIR /opt/omnigrex/pi
COPY agent/pi/package.json agent/pi/package-lock.json ./
COPY agent/pi/package-integrity.mjs agent/pi/retain-packages.mjs ./
RUN npm ci --ignore-scripts --no-audit --no-fund && \
    node retain-packages.mjs && npm cache clean --force
COPY agent/pi/*.mjs ./
COPY --from=bootstrap /out/omnigrex-environment /usr/local/bin/omnigrex-environment
RUN case "$TARGETARCH" in \
      amd64) mise_arch=x64; mise_sha=2af841603ad2981529f230add15c601c64e3a81a79f4f41e9860eb38b7672771; rg_target=x86_64-unknown-linux-musl; rg_sha=1c9297be4a084eea7ecaedf93eb03d058d6faae29bbc57ecdaf5063921491599; fd_target=x86_64-unknown-linux-musl; fd_sha=2b6bfaae8c48f12050813c2ffe1884c61ea26e750d803df9c9114550a314cd14 ;; \
      arm64) mise_arch=arm64; mise_sha=a2f1321f1189f2e1e62b1bd0d20fc59ba3c6a0b38b2c64fcfd29296ca44d2905; rg_target=aarch64-unknown-linux-gnu; rg_sha=2b661c6ef508e902f388e9098d9c4c5aca72c87b55922d94abdba830b4dc885e; fd_target=aarch64-unknown-linux-musl; fd_sha=996b9b1366433b211cb3bbedba91c9dbce2431842144d925428ead0adf32020b ;; \
      *) exit 1 ;; esac && \
    curl -fsSL "https://github.com/jdx/mise/releases/download/v2026.8.10/mise-v2026.8.10-linux-${mise_arch}-musl" -o /usr/local/bin/mise && \
    echo "$mise_sha  /usr/local/bin/mise" | sha256sum -c - && chmod 0755 /usr/local/bin/mise && \
    curl -fsSL "https://github.com/BurntSushi/ripgrep/releases/download/15.1.0/ripgrep-15.1.0-${rg_target}.tar.gz" -o /tmp/rg.tgz && echo "$rg_sha  /tmp/rg.tgz" | sha256sum -c - && \
    curl -fsSL "https://github.com/sharkdp/fd/releases/download/v10.3.0/fd-v10.3.0-${fd_target}.tar.gz" -o /tmp/fd.tgz && echo "$fd_sha  /tmp/fd.tgz" | sha256sum -c - && \
    mkdir /tmp/rg /tmp/fd && tar -xzf /tmp/rg.tgz -C /tmp/rg --strip-components=1 && tar -xzf /tmp/fd.tgz -C /tmp/fd --strip-components=1 && \
    cp /tmp/rg/rg /tmp/fd/fd /usr/local/bin/ && rm -rf /tmp/rg /tmp/fd /tmp/rg.tgz /tmp/fd.tgz && \
    mkdir -p /workspace /state /run/pi-agent /home/opencode/.local/share/mise /home/opencode/.local/share/omnigrex-tool-data && \
    chown -R 10001:10001 /workspace /state /run/pi-agent /home/opencode && \
    test "$(node --version)" = v24.14.1 && test "$(mise --version | cut -d ' ' -f 1)" = 2026.8.10 && git --version
USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/omnigrex-environment", "run", "/usr/local/bin/node", "/opt/omnigrex/pi/runner.mjs"]

FROM golden AS workflow-fixture
COPY agent/pi/qualification/turn.mjs /opt/omnigrex/pi/qualification/turn.mjs
