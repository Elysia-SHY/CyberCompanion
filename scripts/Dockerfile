# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o cybercompanion ./cmd/cybercompanion

# Final runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app

COPY --from=builder /app/cybercompanion /app/cybercompanion
COPY config.example.json /app/config.json

EXPOSE 8088

ENTRYPOINT ["/app/cybercompanion", "-config", "/app/config.json"]
