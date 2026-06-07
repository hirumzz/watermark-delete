# Build stage
FROM golang:alpine AS builder

WORKDIR /build

# Install git for dependency resolution if required
RUN apk add --no-cache git

# Copy files
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Compile binary statically
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o gateway ./cmd/gateway/main.go

# Production stage
FROM alpine:3.19

# Security: run as non-root user
USER nobody

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/gateway /app/gateway

# Expose port (configured via env, default 8080)
EXPOSE 8080

# Environment variables defaults
ENV PORT=8080
ENV REDIS_ADDR=redis:6379
ENV STORAGE_DIR=/app/shared-storage
ENV DOWNLOAD_SIGNING_SECRET=dev-secret-key-change-in-production-12345

CMD ["/app/gateway"]
