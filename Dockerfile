# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# Python 3.13.11 slim-bookworm manifest list, pinned by immutable digest.
ARG PYTHON_IMAGE=python:3.13.11-slim-bookworm@sha256:20080e807bfc404f8450b185cf0fc95d553462673598549613735f70a5b4d5d0
ARG SING_BOX_VERSION=1.14.1
ARG SB2P_VERSION=0.3.4

FROM ${PYTHON_IMAGE} AS builder
ARG SING_BOX_VERSION
ARG SB2P_VERSION
ARG TARGETARCH
ARG TARGETVARIANT

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=20250419~deb12u1 \
        curl=7.88.1-10+deb12u15 \
        tar=1.34+dfsg-1.2+deb12u1 \
    && rm -rf /var/lib/apt/lists/*

# Install the exact PyPI wheel into an isolated prefix. It has no required runtime
# dependencies, so --no-deps also prevents an unpinned transitive install.
RUN python -m pip download --no-cache-dir --no-deps --dest=/tmp/sb2p-wheel \
        "singbox2proxy==${SB2P_VERSION}" \
    && echo "2aea894e996824088b0d6fb970aed55d79c572f5cc32478864da7fcdf9c60151  /tmp/sb2p-wheel/singbox2proxy-${SB2P_VERSION}-py3-none-any.whl" \
        | sha256sum --check --status \
    && python -m pip install --no-cache-dir --no-deps --prefix=/opt/sb2p \
        "/tmp/sb2p-wheel/singbox2proxy-${SB2P_VERSION}-py3-none-any.whl" \
    && rm -rf /tmp/sb2p-wheel

# Bake sing-box into the image at build time. It is never downloaded at container startup.
RUN set -eux; \
    case "${TARGETARCH}${TARGETVARIANT}" in \
      amd64)        sb_arch='amd64'; sb_sha256='12cb2816b52febb356f6a885b740cc8758c3f30b8ae0ca8edba80f0d2d35343f' ;; \
      arm64)        sb_arch='arm64'; sb_sha256='6060b42fa84c5dcaeae1799af7f61b0f1ae4855d9d5ddc9e02baba17154b3ae2' ;; \
      arm/v7|armv7) sb_arch='armv7'; sb_sha256='f2c8af2e3576f40f8ab0d06e1d44840e4eb6bcf410ba8d42381781cb0d6fe41b' ;; \
      arm/v6|armv6) sb_arch='armv6'; sb_sha256='62ab9ef6ae87deaf13f42c91f6a84f1c68fdd920f4e3e941b859da0a54a5c383' ;; \
      386)          sb_arch='386'; sb_sha256='b3126212b32e5b222ae79118618ecffa7d4a1cc56d781973a8af81841359ba02' ;; \
      *) echo "Unsupported Docker target architecture: ${TARGETARCH}${TARGETVARIANT}" >&2; exit 1 ;; \
    esac; \
    base="sing-box-${SING_BOX_VERSION}-linux-${sb_arch}"; \
    curl -fL --retry 5 --retry-delay 2 \
      "https://github.com/SagerNet/sing-box/releases/download/v${SING_BOX_VERSION}/${base}.tar.gz" \
      -o /tmp/sing-box.tar.gz; \
    printf '%s  %s\n' "$sb_sha256" /tmp/sing-box.tar.gz | sha256sum --check --status; \
    mkdir -p /tmp/sing-box; \
    tar -xzf /tmp/sing-box.tar.gz -C /tmp/sing-box --strip-components=1; \
    install -m 0755 /tmp/sing-box/sing-box /usr/local/bin/sing-box; \
    /usr/local/bin/sing-box version

FROM ${PYTHON_IMAGE} AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=20250419~deb12u1 \
        socat=1.7.4.4-2 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /opt/sb2p /usr/local
COPY --from=builder /usr/local/bin/sing-box /usr/local/bin/sing-box
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY supervisor.py /usr/local/bin/supervisor.py

RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh /usr/local/bin/supervisor.py \
    && command -v sb2p \
    && command -v sing-box

RUN addgroup --system sb2p \
    && adduser --system --ingroup sb2p --no-create-home --disabled-login sb2p

# Public container ports. sb2p itself stays on loopback-only private ports;
# socat publishes these two listeners on all container interfaces.
EXPOSE 1080 8080

ENV SB2P_INTERNAL_SOCKS_PORT=11080 \
    SB2P_INTERNAL_HTTP_PORT=18080 \
    PYTHONUNBUFFERED=1

# The health check only verifies that the supervisor process is running. Docker's
# init shim may be PID 1, so find the supervisor in /proc. Upstream reachability
# remains the supervisor's responsibility for selection and failover.
HEALTHCHECK --interval=30s --timeout=15s --start-period=30s --retries=2 \
    CMD ["sh", "-ec", "for task_cmdline in /proc/[0-9]*/cmdline; do [ -r \"$task_cmdline\" ] || continue; if tr '\\000' ' ' < \"$task_cmdline\" | grep -q '[s]upervisor.py'; then exit 0; fi; done; exit 1"]

USER sb2p

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
