ARG BUILD_FROM=ghcr.io/home-assistant/aarch64-base:3.24

# Build (and test) the single Go binary on the target architecture. CGO is needed for SQLite.
FROM golang:1.27-alpine3.24 AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY app/go.mod app/go.sum ./
RUN go mod download && go mod verify
COPY app/ ./
ARG BUILD_VERSION=dev
RUN go vet ./... \
 && go test ./... \
 && CGO_ENABLED=1 go build -trimpath -tags sqlite_omit_load_extension \
      -ldflags "-s -w -X main.Version=${BUILD_VERSION}" -o /out/whatsapp-mcp .

FROM ${BUILD_FROM}
COPY --from=build /out/whatsapp-mcp /usr/bin/whatsapp-mcp
COPY rootfs /
EXPOSE 8120
