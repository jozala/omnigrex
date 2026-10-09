FROM golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY experiments/microsandbox ./experiments/microsandbox
RUN CGO_ENABLED=1 go test -c -tags=integration,microsandbox -o /out/sdk.test ./experiments/microsandbox

FROM ghcr.io/superradcompany/microsandbox@sha256:be3d9f5f99b937ac4c4452b2ff33657a4ef74a117fdb584abb6d3c7db53e248f
COPY --from=build /out/sdk.test /usr/local/bin/sdk.test
USER 10001:10001
ENV HOME=/tmp OMNIGREX_MICROSANDBOX_INTEGRATION=1
ENTRYPOINT ["/usr/local/bin/sdk.test"]
CMD ["-test.v", "-test.run", "^TestSDKProtocolAndCapture$", "-test.timeout", "3m"]
