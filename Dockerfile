# Build stage
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

# Allow Go to download newer toolchains if needed by dependencies
ENV GOTOOLCHAIN=auto

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY *.go ./

# Build the binary with CGO enabled for SQLite
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o skipthefeed .

# Runtime stage
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    ffmpeg \
    python3 \
    py3-pip \
    sqlite \
    tzdata \
    nodejs

# Install yt-dlp and gallery-dl
RUN pip3 install --break-system-packages --no-cache-dir yt-dlp gallery-dl

# Configure yt-dlp to use nodejs for YouTube
RUN mkdir -p /etc/yt-dlp && echo "--js-runtime nodejs" > /etc/yt-dlp/config

# Create non-root user
RUN adduser -D -u 1000 skipthefeed

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/skipthefeed .

# Copy dashboard files
COPY dashboard/ ./dashboard/

# Create data directory structure
RUN mkdir -p /data/config /data/downloads && \
    chown -R skipthefeed:skipthefeed /app /data

# Switch to non-root user
USER skipthefeed

# Environment variables with defaults
ENV SKIPTHEFEED_DATA_DIR=/data \
    SKIPTHEFEED_PORT=3333 \
    SKIPTHEFEED_DASHBOARD_DIR=/app/dashboard

# Expose port
EXPOSE 3333

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:3333/health || exit 1

# Volume for persistent data
VOLUME ["/data"]

# Run the bot
CMD ["./skipthefeed"]
