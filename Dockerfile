FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" -o /tailnet-mcp . && mkdir /state

FROM gcr.io/distroless/static-debian12:nonroot
LABEL io.modelcontextprotocol.server.name="io.github.zedong-peng/tailnet-mcp"
COPY --from=build /tailnet-mcp /tailnet-mcp
# a named volume mounted here inherits the nonroot owner, so node keys survive restarts
COPY --from=build --chown=nonroot:nonroot /state /home/nonroot/.local/state/tailnet-mcp
ENTRYPOINT ["/tailnet-mcp"]
