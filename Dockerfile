# Stage 1: Build React SPA bundle
FROM node:26-alpine AS web-builder
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary embedding migrations and compiled UI
FROM golang:1.27.4-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/
COPY web/embed.go ./web/embed.go
COPY --from=web-builder /app/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/reconcilex ./cmd/reconcilex

# Stage 3: Minimal runtime image
FROM alpine:3.24
RUN adduser -D -u 10001 reconcilex
USER reconcilex
WORKDIR /app
COPY --from=go-builder /out/reconcilex /usr/local/bin/reconcilex
EXPOSE 8080
ENTRYPOINT ["reconcilex"]
