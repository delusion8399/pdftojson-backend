## Multi-stage build: build with golang:alpine, run on minimal alpine
FROM --platform=$BUILDPLATFORM golang:alpine AS builder

WORKDIR /src

# Install CA certs now for module downloads and future use
RUN apk add --no-cache ca-certificates git && update-ca-certificates

# Cache modules first
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# Build a static-ish binary matching the target platform
ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
RUN go build -ldflags="-s -w" -o /bin/server ./main.go

# Final minimal runtime image
FROM --platform=$TARGETPLATFORM alpine:3.20

RUN apk add --no-cache ca-certificates && update-ca-certificates \
    && adduser -D -H -u 10001 appuser

WORKDIR /app
COPY --from=builder /bin/server /app/server

# Required at runtime
ENV GEMINI_API_KEY=""

EXPOSE 7000
USER appuser
ENTRYPOINT ["/app/server"]
