FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./

RUN go mod download

COPY . .

# Build with CGO disabled for alpine/scratch compatibility
RUN CGO_ENABLED=0 go build -o gphotos2immich main.go

# The optional Immich -> Google Photos uploader. Pinned and checksum-verified.
FROM alpine:3.20 AS gotohp

ARG TARGETARCH
ARG GOTOHP_VERSION=v0.11.0
ARG GOTOHP_SHA256=945021d5dbc36a96f53121eb4f907ace39d9457cea1b43107ed1986441b29030

RUN apk add --no-cache ca-certificates wget \
    && if [ "$TARGETARCH" = "amd64" ]; then \
         wget -q -O /gotohp-cli "https://github.com/xob0t/gotohp/releases/download/${GOTOHP_VERSION}/gotohp-cli_amd64" \
         && echo "${GOTOHP_SHA256}  /gotohp-cli" | sha256sum -c - \
         && chmod +x /gotohp-cli; \
       else \
         printf '#!/bin/sh\necho "gotohp-cli is only published for linux/amd64; the Google upload feature is unavailable on this architecture" >&2\nexit 1\n' > /gotohp-cli \
         && chmod +x /gotohp-cli; \
       fi

FROM alpine:latest

# Run as an unprivileged user (matches the default 'user' on most hosts)
WORKDIR /app

# Install ca-certificates and tzdata for timezones, plus su-exec for dropping privileges
RUN apk --no-cache add ca-certificates tzdata su-exec \
    && addgroup -g 1000 app \
    && adduser -u 1000 -G app -D -H app \
    && chown app:app /app

RUN mkdir -p /app/data \
    && chown -R app:app /app/data

COPY --from=builder --chown=app:app /app/gphotos2immich .
COPY --from=gotohp --chown=app:app /gotohp-cli /app/gotohp-cli
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

# gotohp-cli keeps small local caches relative to HOME.
ENV HOME=/app/data

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["./gphotos2immich"]
