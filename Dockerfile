# =============================================================================
# ReconcileX Multi-Stage Build
#
# Produces a single minimal Alpine container holding one static Go binary
# (`reconcilex`) that embeds both SQL migrations and the compiled React SPA.
# =============================================================================


# -----------------------------------------------------------------------------
# Stage 1: Build the React SPA production bundle
# -----------------------------------------------------------------------------
FROM node:26-alpine AS web-builder
WORKDIR /app/web

# Copy package manifests first so `npm ci` stays cached unless dependencies change.
COPY web/package.json web/package-lock.json ./
RUN npm ci

# Copy frontend source files and compile TypeScript + Vite assets into /app/web/dist.
COPY web/ ./
RUN npm run build


# -----------------------------------------------------------------------------
# Stage 2: Compile the static Go binary embedding migrations and web/dist
# -----------------------------------------------------------------------------
FROM golang:1.27.4-alpine AS go-builder
WORKDIR /app

# Download Go modules ahead of copying source files to maximize Docker layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy Go entrypoints, internal packages, embedded SQL migrations, and web embed shim.
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/
COPY web/embed.go ./web/embed.go

# Copy the compiled Vite bundle from Stage 1 so `//go:embed all:dist` packages it.
COPY --from=web-builder /app/web/dist ./web/dist

# Build a CGO-free static Linux binary with `-trimpath` for reproducible build paths.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/reconcilex ./cmd/reconcilex


# -----------------------------------------------------------------------------
# Stage 3: Minimal unprivileged runtime image
# -----------------------------------------------------------------------------
FROM alpine:3.24

# Create and switch to a non-root user so the server runs with least privilege.
RUN adduser -D -u 10001 reconcilex
USER reconcilex
WORKDIR /app

# Install the single self-contained binary and document the default HTTP listen port.
COPY --from=go-builder /out/reconcilex /usr/local/bin/reconcilex
EXPOSE 8080

ENTRYPOINT ["reconcilex"]
