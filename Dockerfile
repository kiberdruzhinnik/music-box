# syntax=docker/dockerfile:1.7

ARG PYTHON_VERSION=3.13
ARG SING_BOX_VERSION=1.14.1
ARG SB2P_REF=main

FROM python:${PYTHON_VERSION}-slim AS builder
ARG SING_BOX_VERSION
ARG SB2P_REF
ARG TARGETARCH
ARG TARGETVARIANT

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git tar \
    && rm -rf /var/lib/apt/lists/*

# Install singbox2proxy from the requested GitHub ref into an isolated prefix.
RUN python -m pip install --no-cache-dir --prefix=/opt/sb2p \
    "git+https://github.com/nichind/singbox2proxy.git@${SB2P_REF}"

# Bake sing-box into the image at build time. It is never downloaded at container startup.
RUN set -eux; \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      amd64)        sb_arch='amd64' ;; \
      arm64)        sb_arch='arm64' ;; \
      arm/v7|armv7) sb_arch='armv7' ;; \
      arm/v6|armv6) sb_arch='armv6' ;; \
      386)          sb_arch='386' ;; \
      *) echo "Unsupported Docker target architecture: ${TARGETARCH}${TARGETVARIANT}" >&2; exit 1 ;; \
    esac; \
    base="sing-box-${SING_BOX_VERSION}-linux-${sb_arch}"; \
    curl -fL --retry 5 --retry-delay 2 \
      "https://github.com/SagerNet/sing-box/releases/download/v${SING_BOX_VERSION}/${base}.tar.gz" \
      -o /tmp/sing-box.tar.gz; \
    mkdir -p /tmp/sing-box; \
    tar -xzf /tmp/sing-box.tar.gz -C /tmp/sing-box --strip-components=1; \
    install -m 0755 /tmp/sing-box/sing-box /usr/local/bin/sing-box; \
    /usr/local/bin/sing-box version

FROM python:${PYTHON_VERSION}-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates socat \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /opt/sb2p /usr/local
COPY --from=builder /usr/local/bin/sing-box /usr/local/bin/sing-box
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY supervisor.py /usr/local/bin/supervisor.py

RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh /usr/local/bin/supervisor.py \
    && command -v sb2p \
    && command -v sing-box

# Public container ports. sb2p itself stays on loopback-only private ports;
# socat publishes these two listeners on all container interfaces.
EXPOSE 1080 8080

ENV SB2P_INTERNAL_SOCKS_PORT=11080 \
    SB2P_INTERNAL_HTTP_PORT=18080 \
    PYTHONUNBUFFERED=1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
