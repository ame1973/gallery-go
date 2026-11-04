# syntax=docker/dockerfile:1
FROM node:24.14.1-bookworm-slim@sha256:b506e7321f176aae77317f99d67a24b272c1f09f1d10f1761f2773447d8da26c AS web
WORKDIR /build
RUN npm install --global pnpm@11.25.0
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/package.json
RUN pnpm install --frozen-lockfile
COPY web web
RUN pnpm build

FROM golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS backend
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY migrations migrations
COPY contracts contracts
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /gallerygo ./cmd/gallerygo

FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3
RUN mkdir -p /data /cache /app/web /app/config && chown -R 65532:65532 /data /cache /app
COPY --from=backend /gallerygo /usr/local/bin/gallerygo
COPY --from=web /build/web/dist /app/web
COPY deploy/config.container.yaml /app/config/config.yaml
USER 65532:65532
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["gallerygo", "healthcheck", "--config", "/app/config/config.yaml"]
ENTRYPOINT ["gallerygo"]
CMD ["serve", "--config", "/app/config/config.yaml"]
