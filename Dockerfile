FROM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY agent/pi ./agent/pi

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/omnigrex ./cmd/omnigrex

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS runtime

ARG TARGETARCH

RUN --mount=type=bind,source=deploy/orchestrator-apk-packages.sha256,target=/tmp/apk-packages.sha256,readonly \
    case "$TARGETARCH" in \
      amd64) apk_arch="x86_64" ;; \
      arm64) apk_arch="aarch64" ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    mkdir -p /tmp/apks && \
    installed=0 && \
    while read -r architecture checksum repository package; do \
      [ "$architecture" = "$TARGETARCH" ] || continue; \
      wget -q "https://dl-cdn.alpinelinux.org/alpine/v3.24/${repository}/${apk_arch}/${package}.apk" -O "/tmp/apks/${package}.apk" || { echo "failed locked package download: ${package}" >&2; exit 1; }; \
      echo "${checksum}  /tmp/apks/${package}.apk" | sha256sum -c - || exit 1; \
      installed=$((installed + 1)); \
    done < /tmp/apk-packages.sha256 && \
    [ "$installed" -eq 38 ] && \
    apk add --no-cache --no-network /tmp/apks/*.apk && \
    rm -rf /tmp/apks && \
    addgroup -g 10001 omnigrex && \
    adduser -D -u 10001 -G omnigrex -h /home/omnigrex omnigrex && \
    mkdir -p /var/lib/omnigrex/workspaces /var/lib/omnigrex/mise /var/lib/omnigrex/provider-auth && \
    chown -R 10001:10001 /var/lib/omnigrex

COPY --from=build --chown=10001:10001 /out/omnigrex /usr/local/bin/omnigrex

LABEL org.opencontainers.image.source="https://github.com/jozala/omnigrex" \
      org.opencontainers.image.title="Omnigrex Orchestrator"

USER 10001:10001
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/omnigrex"]
