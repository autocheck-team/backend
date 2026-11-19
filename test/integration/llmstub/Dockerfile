FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY test/integration/llmstub/*.go ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /stub server.go
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /stub /stub
ENTRYPOINT ["/stub"]
