#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

export PATH="/usr/local/go/bin:$PATH"

go build -o gateway ./cmd/gateway
echo "Built: ./gateway"
