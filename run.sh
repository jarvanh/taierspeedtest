#!/usr/bin/env bash
# TaierSpeedtest 一键运行：下载最新 Release 并启动。无需安装字体或其它依赖。
set -euo pipefail

REPO="${TAIERSPEED_REPO:-MiaM1ku/taierspeedtest}"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "[X] 不支持的架构: $ARCH" >&2; exit 1 ;;
esac
if [ "$OS" != "linux" ]; then
  echo "[X] 当前仅提供 Linux 预编译包" >&2
  exit 1
fi

ASSET="taierspeedtest-${OS}-${ARCH}"
API="https://api.github.com/repos/${REPO}/releases/latest"
echo "[i] 获取 ${REPO} 最新版本..." >&2
JSON="$(curl -fsSL "$API")"
URL="$(printf '%s' "$JSON" | grep -oE "https://[^\"]+/${ASSET}" | head -n1)"
TAG="$(printf '%s' "$JSON" | grep -oE '"tag_name": *"[^"]+"' | head -n1 | cut -d'"' -f4)"
if [ -z "$URL" ]; then
  echo "[X] 未找到 ${ASSET}，请确认仓库已发布 Release: https://github.com/${REPO}/releases" >&2
  exit 1
fi

BIN="${TMPDIR:-/tmp}/taierspeedtest-${TAG:-latest}-${ARCH}"
echo "[i] 下载 ${TAG} ${ASSET}" >&2
curl -fL --retry 3 -o "$BIN" "$URL"
chmod +x "$BIN"
exec "$BIN" "$@"
