FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./

RUN go mod download

COPY . .

# Build with CGO disabled for alpine/scratch compatibility
RUN CGO_ENABLED=0 go build -o gphotos2immich main.go

FROM alpine:latest

WORKDIR /app

# Install ca-certificates and tzdata for timezones
RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /app/gphotos2immich .

CMD ["./gphotos2immich"]
