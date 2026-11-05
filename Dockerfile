# Build stage
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /app/campaign-manager ./cmd/campaign-manager

# Runtime stage
FROM alpine:latest

WORKDIR /app

COPY --from=build /app/campaign-manager .

# The config holds API tokens, so it is mounted at run time, never built in.
USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/app/campaign-manager", "--config", "/config/config.json"]
