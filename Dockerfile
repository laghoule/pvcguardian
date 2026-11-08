# syntax=docker/dockerfile:1
# Build stage
FROM golang:1.27-alpine AS builder

WORKDIR /src

# Cache go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build statically linked binary
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# Ensure ca-certificates and nonroot user/group are prepared for scratch
RUN apk add --no-cache ca-certificates tzdata && \
    echo "nonroot:x:65532:65532:nonroot:/:/sbin/nologin" > /etc/passwd.nonroot && \
    echo "nonroot:x:65532:" > /etc/group.nonroot

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /pvcguardian ./cmd/pvcguardian

# Runtime stage: scratch
FROM scratch

# Copy TLS root certificates, timezone data, and nonroot user definitions
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd.nonroot /etc/passwd
COPY --from=builder /etc/group.nonroot /etc/group

COPY --from=builder /pvcguardian /pvcguardian
COPY LICENSE /LICENSE

USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/pvcguardian"]
CMD ["--config=/etc/pvcguardian/config.yaml"]
