#!/usr/bin/env bash
# 一条命令启动：./run.sh
set -e
cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/go-sdk/go/bin/go" ]; then
    export PATH="$PATH:$HOME/go-sdk/go/bin"
  else
    echo "未找到 Go，请先安装 Go 1.22+（https://go.dev/dl/）" >&2
    exit 1
  fi
fi
export GOTOOLCHAIN=local

echo "启动外卖商户申诉与证据归档系统…"
exec go run ./cmd/server
