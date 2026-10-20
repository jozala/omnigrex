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

RUN apk add --no-cache \
    bash ca-certificates curl git jq gcc musl-dev \
    openssh-client-default ripgrep && \
    mkdir -p /usr/share/omnigrex /var/lib/omnigrex/workspaces /var/lib/omnigrex/mise /var/lib/omnigrex/provider-auth && \
    apk info -v | sort > /usr/share/omnigrex/apk-packages.txt && \
    chmod 644 /usr/share/omnigrex/apk-packages.txt && \
    addgroup -g 10001 omnigrex && \
    adduser -D -u 10001 -G omnigrex -h /home/omnigrex omnigrex && \
    chown -R 10001:10001 /var/lib/omnigrex

COPY --from=build --chown=10001:10001 /out/omnigrex /usr/local/bin/omnigrex

LABEL org.opencontainers.image.source="https://github.com/jozala/omnigrex" \
      org.opencontainers.image.title="Omnigrex Orchestrator"

USER 10001:10001
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/omnigrex"]
