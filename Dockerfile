# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS builder
ARG VERSION=1.3.0
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildDate=${BUILD_DATE}" \
    -o /out/toolprune ./cmd/toolprune

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/toolprune /usr/local/bin/toolprune
RUN printf 'version: "1"\nserver:\n  transport: "stdio"\npruning:\n  tier: "dynamic"\nrouting:\n  default_profile: "full"\n  namespace_mode: "auto"\n  active_window_size: 15\nupstreams: []\n' > /app/toolprune.yaml

ENTRYPOINT ["toolprune"]
CMD ["proxy", "--config", "/app/toolprune.yaml"]
