# Releases supply an exact published golden digest. The local mise task builds
# this development-only tag from the same checkout and target platform first.
ARG OMNIGREX_GOLDEN_IMAGE=omnigrex/development-golden:dev
ARG OMNIGREX_DEVELOPMENT_IMAGE=development
FROM --platform=$BUILDPLATFORM golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS downloads
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# Download all versions in the project graph, including lazy-loaded dependencies.
RUN go mod download all
COPY mise.toml ./
# Include the Go-based development tools' module graphs, without retaining binaries.
RUN deadcode_version="$(sed -n 's/^"go:golang.org\/x\/tools\/cmd\/deadcode" = "\(.*\)"/\1/p' mise.toml)" && \
    test -n "$deadcode_version" && go install "golang.org/x/tools/cmd/deadcode@$deadcode_version"
COPY agent/environments/omnigrex/deb-packages.sha256 /tmp/deb-packages.sha256
RUN test "$TARGETARCH" = amd64 && mkdir /debs && \
    while read -r checksum asset; do \
      case "$checksum" in ''|'#'*) continue ;; esac; \
      file="/debs/${asset##*/}"; \
      curl --fail --silent --show-error --location "https://deb.debian.org/debian/$asset" -o "$file" && \
      echo "$checksum  $file" | sha256sum -c - || exit 1; \
    done < /tmp/deb-packages.sha256
COPY agent/environments/omnigrex/tool-downloads.sha256 /tmp/tool-downloads.sha256
RUN mkdir /tools && \
    while read -r checksum url; do \
      case "$checksum" in ''|'#'*) continue ;; esac; \
      file="/tools/${url##*/}"; \
      curl --fail --silent --show-error --location "$url" -o "$file" && \
      echo "$checksum  $file" | sha256sum -c - || exit 1; \
    done < /tmp/tool-downloads.sha256 && \
    tar -xzf /tools/docker-29.8.1.tgz -C /tools && \
    chmod 0755 /tools/docker-compose-linux-x86_64 /tools/buildx-v0.38.0.linux-amd64

FROM ${OMNIGREX_GOLDEN_IMAGE} AS development
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/jozala/omnigrex" \
      org.opencontainers.image.revision="${VERSION}"
USER 0:0
ARG TARGETARCH
RUN --network=none --mount=type=bind,from=downloads,source=/debs,target=/tmp/debs,readonly \
    test "$TARGETARCH" = amd64 && \
    test "$(dpkg --print-architecture)" = amd64 && \
    test -d /home/omnigrex/.local/share/mise && \
    test -d /home/omnigrex/.local/share/omnigrex-tool-data && \
    . /etc/os-release && test "$ID:$VERSION_ID" = debian:12 && \
    dpkg --install /tmp/debs/*.deb && \
    update-alternatives --set iptables /usr/sbin/iptables-legacy && \
    update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy
COPY --from=downloads /tools/docker/ /usr/local/bin/
COPY --from=downloads /tools/docker-compose-linux-x86_64 /usr/local/lib/docker/cli-plugins/docker-compose
COPY --from=downloads /tools/buildx-v0.38.0.linux-amd64 /usr/local/lib/docker/cli-plugins/docker-buildx
COPY --from=downloads /go/pkg/mod/cache/download/ /opt/omnigrex/go-proxy/
RUN chmod -R a-w /opt/omnigrex/go-proxy && \
    test "$(docker --version | cut -d ' ' -f 3)" = '29.8.1,' && \
    docker compose version && docker buildx version && \
    test "$(jq --version)" = jq-1.6 && gcc --version && make --version
USER 10001:10001
WORKDIR /workspace

# Disposable cache tests also carry Go, but the published development image does not.
FROM downloads AS cache-test-build
COPY agent/environments/omnigrex/*_test.go ./agent/environments/omnigrex/
RUN GOOS=linux GOARCH=$TARGETARCH go test -c -o /out/cache.test ./agent/environments/omnigrex

FROM golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS qualification-go

FROM ${OMNIGREX_DEVELOPMENT_IMAGE} AS cache-qualification
COPY --from=qualification-go /usr/local/go/ /opt/qualification/go/
COPY --from=cache-test-build /out/cache.test /opt/qualification/cache.test
COPY go.mod go.sum /opt/qualification/module/
COPY scripts/development-environment.sh /opt/qualification/development-environment.sh
COPY scripts/go-test.sh /opt/qualification/go-test.sh
ENTRYPOINT ["/opt/qualification/cache.test", "-test.v"]

# Catch repository test-tool dependencies before publishing a candidate image.
FROM cache-qualification AS repository-qualification
COPY . /workspace
COPY --from=development-docs /operator-guide.md /workspace/docs/operator-guide.md
ENV PATH=/opt/qualification/go/bin:/usr/local/bin:/usr/bin:/bin \
    HOME=/tmp GOPATH=/var/tmp/go-modules GOCACHE=/var/tmp/go-build \
    GOPROXY=file:///opt/omnigrex/go-proxy,https://proxy.golang.org,direct \
    GOFLAGS="-p=1 -modcacherw" GOMAXPROCS=2
ENTRYPOINT ["/bin/sh", "scripts/go-test.sh", "-race", "./..."]
