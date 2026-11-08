# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/toolprune ./cmd/toolprune

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/toolprune /usr/local/bin/toolprune
RUN printf 'version: "1"\nserver:\n  transport: "stdio"\npruning:\n  tier: "dynamic"\nrouting:\n  default_profile: "full"\n  namespace_mode: "auto"\n  active_window_size: 15\nupstreams: []\n' > /app/toolprune.yaml

ENTRYPOINT ["toolprune"]
CMD ["proxy", "--config", "/app/toolprune.yaml"]
