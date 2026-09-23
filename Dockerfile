# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS builder
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILDARCH
WORKDIR /src

# Bake the exact sing-box release into the image, with per-architecture checksums.
RUN set -eux; \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      amd64) sb_arch='amd64-musl'; sb_sha256='b907365b154e4a7e3e40be15c2cd83433c0fa65c7dc736bdb1b5face2afe4501' ;; \
      arm64) sb_arch='arm64-musl'; sb_sha256='d94fc9704372ca2fa2854e54c20b406e4b8779b5ccdd0c557da90ea9344e9631' ;; \
      *) echo "Unsupported Docker target architecture: ${TARGETARCH}${TARGETVARIANT}; only amd64 and arm64 are supported" >&2; exit 1 ;; \
    esac; \
    base="sing-box-1.14.1-linux-${sb_arch}"; \
    curl -fL --retry 5 --retry-delay 2 \
      "https://github.com/SagerNet/sing-box/releases/download/v1.14.1/${base}.tar.gz" \
      -o /tmp/sing-box.tar.gz; \
    printf '%s  %s\n' "$sb_sha256" /tmp/sing-box.tar.gz | sha256sum --check --status; \
    mkdir -p /tmp/sing-box /out/tmp; \
    chmod 1777 /out/tmp; \
    tar -xzf /tmp/sing-box.tar.gz -C /tmp/sing-box --strip-components=1; \
    install -m 0755 /tmp/sing-box/sing-box /out/sing-box
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" GOARM="${TARGETVARIANT#v}" \
    go build -trimpath -ldflags='-s -w' -o /out/go-singbox2proxy .
RUN if [ "$TARGETARCH" = "$BUILDARCH" ]; then \
      PATH="/out:${PATH}" go test ./...; \
    else \
      go test ./...; \
    fi

FROM scratch
COPY --from=builder /out/go-singbox2proxy /usr/local/bin/go-singbox2proxy
COPY --from=builder /out/sing-box /usr/local/bin/sing-box
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chmod=1777 --from=builder /out/tmp /tmp
ENV PATH=/usr/local/bin TMPDIR=/dev/shm
USER 65532:65532
EXPOSE 1080 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=2 \
    CMD ["/usr/local/bin/go-singbox2proxy", "--healthcheck"]
ENTRYPOINT ["/usr/local/bin/go-singbox2proxy"]
