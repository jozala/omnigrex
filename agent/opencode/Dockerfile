# check=skip=SecretsUsedInArgOrEnv
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS download

ARG TARGETARCH
ARG OPENCODE_VERSION=1.18.29
ARG MISE_VERSION=2026.8.10

RUN case "$TARGETARCH" in \
      amd64) \
        opencode_arch="x64"; \
        mise_arch="x64"; \
        mise_sha="2af841603ad2981529f230add15c601c64e3a81a79f4f41e9860eb38b7672771" \
        ;; \
      arm64) \
        opencode_arch="arm64"; \
        mise_arch="arm64"; \
        mise_sha="a2f1321f1189f2e1e62b1bd0d20fc59ba3c6a0b38b2c64fcfd29296ca44d2905" \
        ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    case "${OPENCODE_VERSION}:${TARGETARCH}" in \
      1.18.19:amd64) opencode_sha="71eaeb20f2dc55a6d22ea1dabf659db513d02cd65f58b4a29e4c9f45b0d529ac" ;; \
      1.18.19:arm64) opencode_sha="cf5732a2665da6db7331fd4bff37a32eed71d684fef5e0956c0e29254600136b" ;; \
      1.18.29:amd64) opencode_sha="294bfa0f4647e0fc5d3edceed94e8dc1047757975689f4269aadb91d6774f9ed" ;; \
      1.18.29:arm64) opencode_sha="9b9044ede6449dc34888b85f0a51be5777a5886fe5e1cbd67898b13f8a5bec04" ;; \
      *) echo "unsupported OpenCode version and architecture: ${OPENCODE_VERSION}:${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    wget -q "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${opencode_arch}-musl.tar.gz" -O /tmp/opencode.tar.gz && \
    echo "${opencode_sha}  /tmp/opencode.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin && \
    wget -q "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${mise_arch}-musl" -O /usr/local/bin/mise && \
    echo "${mise_sha}  /usr/local/bin/mise" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/opencode /usr/local/bin/mise

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

ARG OPENCODE_VERSION=1.18.29
ARG MISE_VERSION=2026.8.10
ARG TARGETARCH

RUN --mount=type=bind,source=agent/opencode/apk-packages.sha256,target=/tmp/apk-packages.sha256,readonly \
    case "$TARGETARCH" in \
      amd64) apk_arch="x86_64" ;; \
      arm64) apk_arch="aarch64" ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    mkdir -p /tmp/apks && \
    installed=0 && \
    while read -r architecture checksum repository package; do \
      [ "$architecture" = "$TARGETARCH" ] || continue; \
      wget -q "https://dl-cdn.alpinelinux.org/alpine/v3.24/${repository}/${apk_arch}/${package}.apk" -O "/tmp/apks/${package}.apk" || exit 1; \
      echo "${checksum}  /tmp/apks/${package}.apk" | sha256sum -c - || exit 1; \
      installed=$((installed + 1)); \
    done < /tmp/apk-packages.sha256 && \
    [ "$installed" -eq 27 ] && \
    apk add --no-cache --no-network /tmp/apks/*.apk && \
    rm -rf /tmp/apks && \
    addgroup -g 10001 opencode && \
    adduser -D -u 10001 -G opencode -h /home/opencode opencode && \
    mkdir -p \
      /home/opencode/.cache/opencode \
      /home/opencode/.config/opencode \
      /home/opencode/.local/share/mise \
      /home/opencode/.local/share/opencode \
      /home/opencode/.local/state/opencode \
      /home/opencode/.opencode \
      /tmp/opencode \
      /workspace && \
    chown -R 10001:10001 /home/opencode /tmp/opencode /workspace

COPY --from=download /usr/local/bin/opencode /usr/local/bin/opencode
COPY --from=download /usr/local/bin/mise /usr/local/bin/mise

ENV HOME=/home/opencode \
    XDG_CONFIG_HOME=/home/opencode/.config \
    XDG_DATA_HOME=/home/opencode/.local/share \
    XDG_STATE_HOME=/home/opencode/.local/state \
    XDG_CACHE_HOME=/home/opencode/.cache \
    TMPDIR=/tmp/opencode \
    MISE_DATA_DIR=/home/opencode/.local/share/mise \
    OPENCODE_AUTH_CONTENT={} \
    OPENCODE_CONFIG_CONTENT="{\"autoupdate\":false,\"share\":\"disabled\",\"mcp\":{}}" \
    OPENCODE_DISABLE_AUTOUPDATE=1 \
    OPENCODE_DISABLE_SHARE=1 \
    OPENCODE_AUTO_SHARE=false

LABEL org.opencontainers.image.source="https://github.com/jozala/omnigrex" \
      org.opencontainers.image.title="Omnigrex OpenCode Runtime Process" \
      org.opencontainers.image.version="${OPENCODE_VERSION}" \
      io.omnigrex.mise.version="${MISE_VERSION}"

USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["opencode"]
CMD ["acp"]
