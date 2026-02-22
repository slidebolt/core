# --- Build Stage ---
FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src

# Explicit build mode: "prod" (default) or "local"
ARG BUILD_MODE=prod

# Copy core source
COPY . .

# Explicitly handle build modes
RUN if [ "$BUILD_MODE" = "local" ]; then \
        echo "Building in LOCAL mode using staged siblings..." && \
        rm -f go.work && \
        go work init . ./.stage/plugin-sdk ./.stage/plugin-framework ./.stage/plugin-loader; \
    else \
        echo "Building in PROD mode using remote modules..."; \
    fi

# Build the standalone binary
RUN go build -o /app/core ./cmd/main.go

# --- Run Stage ---
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/core /app/core
RUN mkdir -p /app/plugins

# Default ports
EXPOSE 4222 49800

CMD ["/app/core"]
