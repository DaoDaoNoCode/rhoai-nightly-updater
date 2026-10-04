FROM node:22-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
# Type errors or failing frontend tests stop the build, so CI never publishes them.
RUN npm run typecheck && npm test
RUN NODE_OPTIONS="--max-old-space-size=2048" npm run build

FROM golang:1.24-alpine AS backend
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY main.go ./
COPY pkg/ ./pkg/
RUN go test ./...
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server .

FROM registry.access.redhat.com/ubi9/ubi-minimal:9.6
WORKDIR /app
COPY --from=backend /app/server .
COPY --from=frontend /app/frontend/dist ./static
ENV STATIC_DIR=/app/static
ENV PORT=8080
EXPOSE 8080
USER 1001
ENTRYPOINT ["./server"]
