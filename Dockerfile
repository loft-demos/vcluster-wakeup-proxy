# syntax=docker/dockerfile:1.7

########## Build ##########
FROM golang:1.22 AS build
# buildx sets these automatically
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

WORKDIR /src
COPY cmd/proxy/ ./cmd/proxy/

# smaller binary
RUN go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy

########## Runtime ##########
FROM gcr.io/distroless/base-debian12:nonroot
WORKDIR /app
COPY --from=build /out/proxy /app/proxy
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/proxy"]
