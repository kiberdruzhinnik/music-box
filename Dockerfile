# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS cronet
ARG TARGETARCH
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) digest='c3949c6ad64e1d8fcd1e3b1fae4e302b2e553d769665a4bd7576483564c3f026' ;; \
      arm64) digest='8f13a6186aca498d37ee5e1f410282f587d663995aca60d6bf29a2d4f5536f2b' ;; \
      *) exit 1 ;; \
    esac; \
    curl -fL --retry 5 --retry-delay 2 \
      "https://github.com/SagerNet/cronet-go/releases/download/v150.0.7871.63-2/libcronet-linux-${TARGETARCH}.so" \
      -o /tmp/libcronet.so; \
    printf '%s  %s\n' "$digest" /tmp/libcronet.so | sha256sum --check --status

FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS builder
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILDARCH
WORKDIR /src

# The sing-box library is linked into the supervisor from go.mod.
RUN set -eux; \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      amd64|arm64) ;; \
      *) echo "Unsupported Docker target architecture: ${TARGETARCH}${TARGETVARIANT}; only amd64 and arm64 are supported" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out
COPY go.mod go.sum ./
COPY *.go ./
ARG SING_BOX_TAGS=with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale,with_ccm,with_ocm,with_cloudflared,with_naive_outbound,with_usbip,with_openvpn,with_openconnect,badlinkname,tfogo_checklinkname0,with_purego
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -tags "$SING_BOX_TAGS" \
      -ldflags='-s -w -buildid= -X github.com/sagernet/sing-box/constant.Version=1.14.1' \
      -o /out/singbox2proxy-docker .
COPY --from=cronet /tmp/libcronet.so /out/libcronet.so
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    if [ "$TARGETARCH" = "$BUILDARCH" ]; then \
      LD_LIBRARY_PATH=/out SINGBOX_INTEGRATION_TEST=1 CGO_ENABLED=0 \
        go test -tags "$SING_BOX_TAGS" ./...; \
    else \
      go test ./...; \
    fi

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=builder /out/singbox2proxy-docker /usr/local/bin/singbox2proxy-docker
COPY --from=cronet /tmp/libcronet.so /usr/local/bin/libcronet.so
ENV PATH=/usr/local/bin
USER 65532:65532
EXPOSE 1080 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=2 \
    CMD ["/usr/local/bin/singbox2proxy-docker", "--healthcheck"]
ENTRYPOINT ["/usr/local/bin/singbox2proxy-docker"]
