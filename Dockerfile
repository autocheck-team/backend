# Один Dockerfile на все сервисы бэкенда: нужный выбирается через --build-arg CMD=api|worker|migrate.
# Сборка идёт на платформе сборщика с кросс-компиляцией Go, поэтому arm64-образ
# собирается без эмуляции QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD=api
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
