# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /workspace

# Copy go.mod and go.sum first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY cmd/ cmd/
COPY pkg/ pkg/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o kausality-webhook ./cmd/kausality-webhook

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/kausality-webhook .

USER 65532:65532

ENTRYPOINT ["/kausality-webhook"]
