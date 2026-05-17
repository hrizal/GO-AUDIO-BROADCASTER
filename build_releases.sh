#!/bin/bash
# Cross-compile script for Go Audio Streamer

APP_NAME="streamer"
BUILD_DIR="./releases"

# Create build directory
mkdir -p $BUILD_DIR

echo "=== Building Go Audio Streamer for multiple platforms ==="

# Linux AMD64
echo "[1/5] Building for Linux (amd64)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ${BUILD_DIR}/${APP_NAME}-linux-amd64 .

# Linux ARM64
echo "[2/5] Building for Linux (arm64)..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ${BUILD_DIR}/${APP_NAME}-linux-arm64 .

# Windows AMD64
echo "[3/5] Building for Windows (amd64)..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o ${BUILD_DIR}/${APP_NAME}-windows-amd64.exe .

# macOS AMD64 (Intel)
echo "[4/5] Building for macOS (amd64)..."
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o ${BUILD_DIR}/${APP_NAME}-darwin-amd64 .

# macOS ARM64 (Apple Silicon)
echo "[5/5] Building for macOS (arm64)..."
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o ${BUILD_DIR}/${APP_NAME}-darwin-arm64 .

echo ""
echo "=== Build Complete! ==="
ls -lh $BUILD_DIR
