#!/bin/sh
set -eu

scan_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
cd "$scan_dir"

for scanner in semgrep trivy; do
    if ! command -v "$scanner" >/dev/null 2>&1; then
        printf '%s is required for security-scan.sh\n' "$scanner" >&2
        exit 2
    fi
done

image=${1:-local/singbox2proxy-docker:1.0.0}

semgrep scan --metrics=off --error \
    --config p/default \
    --config p/security-audit \
    --config p/owasp-top-ten \
    --exclude .env .

trivy fs --scanners vuln,misconfig,secret \
    --skip-files .env --exit-code 1 .

trivy image --scanners vuln,misconfig,secret \
    --exit-code 1 "$image"
