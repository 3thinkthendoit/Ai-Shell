#!/usr/bin/env bash
# Ai-Shell build script
# Usage:
#   ./scripts/build.sh            # build Windows (default)
#   ./scripts/build.sh windows    # build Windows
#   ./scripts/build.sh mac        # build macOS
#   ./scripts/build.sh mac run    # build macOS and launch
#   ./scripts/build.sh dev        # development mode (hot reload)
#
# NOTE: Wails depends on the platform-native WebView, so each target can only
# be built ON that platform (mac builds on macOS, windows builds on Windows /
# Git Bash / MSYS). Cross-compiling between them is not supported.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

APP_NAME="Ai-Shell"
TARGET="${1:-windows}"
ACTION="${2:-build}"

command -v go >/dev/null 2>&1 || { echo "[ERROR] Go is not installed"; exit 1; }
command -v npm >/dev/null 2>&1 || { echo "[ERROR] npm is not installed"; exit 1; }

# Ensure wails (installed via `go install`) is on PATH
export PATH="$PATH:$(go env GOPATH)/bin"

command -v wails >/dev/null 2>&1 || {
  echo "[INFO] Installing wails CLI..."
  go install github.com/wailsapp/wails/v2/cmd/wails@latest \
    || { echo "[ERROR] Failed to install wails CLI"; exit 1; }
}

# Frontend deps
if [ ! -d frontend/node_modules ]; then
  echo "[INFO] Installing frontend dependencies..."
  (cd frontend && npm install)
fi

if [ "$TARGET" = "dev" ]; then
  wails dev
  exit 0
fi

# Host detection: Darwin = macOS; MINGW*/MSYS* = Windows under Git Bash
HOST_OS="$(uname -s)"

case "$TARGET" in
  windows)
    case "$HOST_OS" in
      MINGW*|MSYS*|CYGWIN*) ;;
      *)
        echo "[ERROR] Windows builds must run on Windows (Git Bash/MSYS)."
        echo "        Cross-compiling from '$HOST_OS' is not supported by Wails."
        exit 1
        ;;
    esac
    echo "[INFO] Building for Windows..."
    # -webview2 embed: bundle WebView2 bootstrapper for machines without WebView2
    wails build -clean -trimpath -webview2 embed -platform windows/amd64
    BIN="$ROOT_DIR/build/bin/$APP_NAME.exe"
    ;;
  mac|darwin)
    [ "$HOST_OS" = "Darwin" ] || {
      echo "[ERROR] macOS builds must run on macOS."
      echo "        Cross-compiling from '$HOST_OS' is not supported by Wails."
      exit 1
    }
    echo "[INFO] Building for macOS..."
    wails build -clean -trimpath -platform darwin/universal
    BIN="$ROOT_DIR/build/bin/$APP_NAME.app"
    ;;
  *)
    echo "Unknown target: $TARGET (use: windows | mac | dev)"
    exit 1
    ;;
esac

if [ ! -e "$BIN" ]; then
  echo "[ERROR] Build output not found: $BIN"
  exit 1
fi

echo "[OK] Build finished: $ROOT_DIR/build/bin/"

if [ "$ACTION" = "run" ]; then
  echo "[INFO] Launching $APP_NAME..."
  if [ -d "$ROOT_DIR/build/bin/$APP_NAME.app" ]; then
    open "$ROOT_DIR/build/bin/$APP_NAME.app"
  else
    "$BIN" &
  fi
fi
