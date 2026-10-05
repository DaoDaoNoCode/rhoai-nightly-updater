# Base images are pinned by digest (multi-arch index); bump them together
# with the tag in the comment. Go 1.27 is a supported release
# (https://go.dev/doc/devel/release); UBI 9.8 is the current UBI 9 minor.
# node:22-alpine
FROM docker.io/library/node:22-alpine@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402 AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
# Type errors, lint errors or failing frontend tests stop the build, so CI
# never publishes them.
RUN npm run typecheck && npm run lint && npm test
RUN NODE_OPTIONS="--max-old-space-size=2048" npm run build

# golang:1.27.1-alpine
FROM docker.io/library/golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS backend
ARG GIT_SHA=unknown
ARG VERSION=dev
ARG BUILD_DATE=unknown
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY main.go ./
COPY pkg/ ./pkg/
COPY deploy/template.yaml ./deploy/template.yaml
RUN go vet ./... && go test ./...
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/juntwang/rhoai-nightly-updater/pkg/api.Version=${VERSION} \
      -X github.com/juntwang/rhoai-nightly-updater/pkg/api.Commit=${GIT_SHA} \
      -X github.com/juntwang/rhoai-nightly-updater/pkg/api.BuildDate=${BUILD_DATE}" \
    -o server .

# ubi9/ubi-minimal:9.8-1790754119
FROM registry.access.redhat.com/ubi9/ubi-minimal@sha256:1d7c5517a4a1a8e2688620b39ee980e82505ca1ab7ae5541b5463120ae9b3897
ARG GIT_SHA=unknown
ARG VERSION=dev
ARG BUILD_DATE=unknown
# Override the labels inherited from the UBI base, which describe UBI.
LABEL org.opencontainers.image.title="rhoai-nightly-updater" \
      org.opencontainers.image.revision="${GIT_SHA}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater" \
      name="rhoai-nightly-updater" \
      vcs-ref="${GIT_SHA}" \
      version="${VERSION}"
WORKDIR /app
COPY --from=backend /app/server .
COPY --from=frontend /app/frontend/dist ./static
ENV STATIC_DIR=/app/static
ENV PORT=8080
EXPOSE 8080 9090
USER 1001
ENTRYPOINT ["./server"]
