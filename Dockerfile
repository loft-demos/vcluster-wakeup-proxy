# syntax=docker/dockerfile:1.7

########## Build ##########
FROM golang:1.22 AS build
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

WORKDIR /src
# copy module files first for better caching
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download || true

# now copy the actual source
COPY cmd/proxy/ ./cmd/proxy/

# build the binary
RUN --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy

########## Runtime ##########
FROM gcr.io/distroless/base-debian12:nonroot
WORKDIR /app
COPY --from=build /out/proxy /app/proxy
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/proxy"]
