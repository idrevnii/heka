# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build \
    -mod=vendor \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/heka \
    ./cmd/heka

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 heka \
    && adduser -S -D -H -u 65532 -G heka heka

COPY --from=build /out/heka /usr/local/bin/heka
COPY deploy/heka.yaml /etc/heka/heka.yaml

USER 65532:65532

EXPOSE 8787

ENTRYPOINT ["/usr/local/bin/heka"]
CMD ["-config", "/etc/heka/heka.yaml", "-log-json"]
