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
    && adduser -S -D -H -u 65532 -G heka heka \
    && mkdir -p /etc/heka \
    && chown 65532:65532 /etc/heka

COPY --from=build /out/heka /usr/local/bin/heka
# Only a first-boot seed: the live config is /etc/heka/heka.yaml, expected
# to be a persistent volume so it survives image rebuilds/redeploys. If it's
# missing at startup it's copied from here (see -seed below); once it
# exists, this seed is never consulted again.
COPY deploy/heka.yaml /usr/share/heka/heka.default.yaml

USER 65532:65532

EXPOSE 8787

ENTRYPOINT ["/usr/local/bin/heka"]
CMD ["-config", "/etc/heka/heka.yaml", "-seed", "/usr/share/heka/heka.default.yaml", "-log-json"]
