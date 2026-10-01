# ── Build stage ──────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk --no-cache add build-base

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_ENV=dev

# Dev build: keep debug symbols
# Prod build: strip symbols, trimpath for reproducibility
RUN if [ "$BUILD_ENV" = "prod" ]; then \
      CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o server .; \
    else \
      CGO_ENABLED=0 go build -o server .; \
    fi

# ── Production stage ─────────────────────────────
FROM alpine:3.21

RUN apk --no-cache add ca-certificates

WORKDIR /app
COPY --from=builder /app/server .

# OpsAI VCS metadata — lets MW map errors to commits
ARG VCS_COMMIT_SHA=""
ARG VCS_REPOSITORY_URL=""
ENV MW_VCS_COMMIT_SHA=${VCS_COMMIT_SHA}
ENV MW_VCS_REPOSITORY_URL=${VCS_REPOSITORY_URL}

CMD ["./server"]
