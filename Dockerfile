FROM golang:1.23-alpine AS builder
WORKDIR /app
RUN apk add --no-cache ca-certificates git

# Copy go.mod first for layer caching
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /kube-actions-runner ./cmd/kube-actions-runner

# Distroless for minimal attack surface
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /kube-actions-runner /kube-actions-runner

# 65532 is nonroot user in distroless
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/kube-actions-runner"]
