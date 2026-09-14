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

# Install ca-certificates and tzdata for timezones
RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -g 1000 app \
    && adduser -u 1000 -G app -D -H app \
    && chown app:app /app

COPY --from=builder --chown=app:app /app/gphotos2immich .

USER app

CMD ["./gphotos2immich"]
