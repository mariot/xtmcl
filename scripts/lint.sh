#!/usr/bin/env bash

set -euo pipefail

if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "golangci-lint is not installed."
  echo "Install with: brew install golangci-lint"
  exit 1
fi

golangci-lint run ./...

