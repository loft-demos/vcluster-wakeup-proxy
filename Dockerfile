# Dockerfile
FROM golang:1.22 AS build
WORKDIR /src
COPY cmd/proxy/ ./cmd/proxy/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/proxy ./cmd/proxy

FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=build /out/proxy /app/proxy
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/proxy"]
