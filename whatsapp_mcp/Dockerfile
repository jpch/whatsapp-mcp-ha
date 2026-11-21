ARG BUILD_FROM=ghcr.io/home-assistant/aarch64-base:3.24-2026.11.0@sha256:48408ed8587f35994babee9ff7ef3e6c6fd3f87ea07fb1f4b11ff1c42401e92a

# Build (and test) the single Go binary on the target architecture. CGO is needed for SQLite.
FROM golang:1.27.4-alpine3.24@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
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
