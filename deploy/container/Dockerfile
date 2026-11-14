FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
ARG RESCUARR_VERSION=dev
ARG RESCUARR_REVISION=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=$RESCUARR_VERSION -X main.commit=$RESCUARR_REVISION" \
      -o /out/rescuarr ./cmd/rescuarr

FROM alpine:3.24.4@sha256:3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a
RUN addgroup -S -g 10001 rescuarr \
    && adduser -S -D -H -u 10001 -G rescuarr -h /data rescuarr \
    && mkdir -m 0700 /data \
    && chown 10001:10001 /data \
    && test -s /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chmod=0555 /out/rescuarr /usr/local/bin/rescuarr
ARG RESCUARR_VERSION=dev
ARG RESCUARR_REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/rnodern/rescuarr" \
      org.opencontainers.image.version="$RESCUARR_VERSION" \
      org.opencontainers.image.revision="$RESCUARR_REVISION"
ENV RESCUARR_LISTEN=0.0.0.0:8787 \
    RESCUARR_DATA_DIR=/data
USER 10001:10001
WORKDIR /data
EXPOSE 8787
HEALTHCHECK --interval=15s --timeout=4s --start-period=10s --retries=3 \
  CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8787/readyz"]
ENTRYPOINT ["/usr/local/bin/rescuarr"]
CMD ["serve"]
