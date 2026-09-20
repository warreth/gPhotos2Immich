FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./

RUN go mod download

COPY . .

# Build with CGO disabled for alpine/scratch compatibility
RUN CGO_ENABLED=0 go build -o gphotos2immich main.go

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
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["./gphotos2immich"]
