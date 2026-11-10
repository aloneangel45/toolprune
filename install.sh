#!/usr/bin/env bash
# ==============================================================================
# toolprune - Official Universal Installer (Linux & macOS)
# High-Performance Model Context Protocol (MCP) Proxy & Dynamic Schema Pruner
# Repository: https://github.com/aloneangel45/toolprune
# ==============================================================================

set -euo pipefail

REPO="aloneangel45/toolprune"
BINARY_NAME="toolprune"

# Color support
if [ -t 1 ]; then
    BOLD="\033[1m"
    GREEN="\033[32m"
    BLUE="\033[34m"
    CYAN="\033[36m"
    YELLOW="\033[33m"
    RED="\033[31m"
    RESET="\033[0m"
else
    BOLD=""
    GREEN=""
    BLUE=""
    CYAN=""
    YELLOW=""
    RED=""
    RESET=""
fi

log_info() {
    printf "${BLUE}[INFO]${RESET} %s\n" "$1"
}

log_success() {
    printf "${GREEN}[SUCCESS]${RESET} %s\n" "$1"
}

log_warn() {
    printf "${YELLOW}[WARN]${RESET} %s\n" "$1"
}

log_error() {
    printf "${RED}[ERROR]${RESET} %s\n" "$1" >&2
}

download_file() {
    local url="$1"
    local dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "${url}" -o "${dest}"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "${dest}" "${url}"
    else
        log_error "Neither curl nor wget found in PATH. Please install one of them."
        exit 1
    fi
}

compute_sha256() {
    local file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "${file}" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "${file}" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "${file}" | awk '{print $NF}'
    else
        log_error "No SHA-256 utility found (sha256sum, shasum, or openssl required)."
        exit 1
    fi
}

# 1. Detect OS
OS_RAW="$(uname -s)"
case "${OS_RAW}" in
    Linux*)     OS="linux" ;;
    Darwin*)    OS="darwin" ;;
    *)
        log_error "Unsupported Operating System: ${OS_RAW}. toolprune supports Linux and macOS (Darwin)."
        exit 1
        ;;
esac

# 2. Detect Architecture
ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
    x86_64|amd64)    ARCH="amd64" ;;
    arm64|aarch64)   ARCH="arm64" ;;
    *)
        log_error "Unsupported architecture: ${ARCH_RAW}. toolprune supports amd64 and arm64."
        exit 1
        ;;
esac

log_info "Detected target platform: ${BOLD}${OS}/${ARCH}${RESET}"

# 3. Resolve latest release version
log_info "Fetching latest release version from GitHub..."
TAG=""
if command -v curl >/dev/null 2>&1; then
    TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4 || true)
elif command -v wget >/dev/null 2>&1; then
    TAG=$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4 || true)
fi

if [ -z "${TAG}" ]; then
    TAG="v1.2.0"
    log_warn "GitHub API rate limit reached or offline; falling back to release ${TAG}"
fi

log_info "Target release: ${BOLD}${TAG}${RESET}"

# 4. Prepare Download URL and Temp Directory
ARCHIVE_NAME="toolprune-${OS}-${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG}/${ARCHIVE_NAME}"
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/checksums.txt"

TMP_DIR="$(mktemp -d)"
cleanup() {
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

log_info "Downloading ${DOWNLOAD_URL}..."
download_file "${DOWNLOAD_URL}" "${TMP_DIR}/${ARCHIVE_NAME}"

log_info "Downloading SHA-256 checksums from ${CHECKSUMS_URL}..."
download_file "${CHECKSUMS_URL}" "${TMP_DIR}/checksums.txt"

# 5. Verify SHA-256 Checksum
EXPECTED_HASH=$(awk -v target="${ARCHIVE_NAME}" '{
    fname = $2
    sub(/^\*/, "", fname)
    if (fname == target) {
        print tolower($1)
        exit
    }
}' "${TMP_DIR}/checksums.txt")

if [ -z "${EXPECTED_HASH}" ]; then
    log_error "Checksum entry for ${ARCHIVE_NAME} not found in checksums.txt"
    exit 1
fi

ACTUAL_HASH=$(compute_sha256 "${TMP_DIR}/${ARCHIVE_NAME}" | tr '[:upper:]' '[:lower:]')
if [ "${ACTUAL_HASH}" != "${EXPECTED_HASH}" ]; then
    log_error "SHA-256 checksum mismatch for ${ARCHIVE_NAME} (expected ${EXPECTED_HASH}, got ${ACTUAL_HASH})"
    exit 1
fi
log_info "SHA-256 checksum verified: ${ACTUAL_HASH}"

# 6. Extract Archive
log_info "Extracting ${ARCHIVE_NAME}..."
tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "${TMP_DIR}"

if [ ! -f "${TMP_DIR}/${BINARY_NAME}" ]; then
    log_error "Extraction failed: ${BINARY_NAME} binary not found in archive."
    exit 1
fi

chmod +x "${TMP_DIR}/${BINARY_NAME}"

# 7. Determine Installation Target Directory
if [ -w "/usr/local/bin" ] || [ "$(id -u)" -eq 0 ]; then
    INSTALL_DIR="/usr/local/bin"
    SUDO=""
    if [ ! -w "/usr/local/bin" ] && command -v sudo >/dev/null 2>&1; then
        SUDO="sudo"
    fi
    ${SUDO} mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
else
    INSTALL_DIR="${HOME}/.local/bin"
    mkdir -p "${INSTALL_DIR}"
    mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
fi

log_success "toolprune installed successfully to ${BOLD}${INSTALL_DIR}/${BINARY_NAME}${RESET}"

# 8. Check PATH
case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
        log_warn "${INSTALL_DIR} is not in your current PATH."
        printf "  Add it to your shell configuration:\n"
        printf "    ${BOLD}export PATH=\"%s:\$PATH\"${RESET}\n\n" "${INSTALL_DIR}"
        ;;
esac

# 9. Success Banner
printf "\n"
printf "${CYAN}+--------------------------------------------------------------+${RESET}\n"
printf "${CYAN}|${RESET}  ${BOLD}toolprune is ready to optimize your LLM agent context.${RESET}      ${CYAN}|${RESET}\n"
printf "${CYAN}+--------------------------------------------------------------+${RESET}\n"
printf "\n"
printf "  Quick Start:\n"
printf "    ${BOLD}toolprune proxy --help${RESET}                 # Inspect available commands\n"
printf "    ${BOLD}toolprune proxy --preset balanced${RESET}      # Run proxy with balanced pruning\n"
printf "    ${BOLD}toolprune proxy --preset strict${RESET}        # Maximum token savings\n"
printf "\n"
printf "  Documentation & Benchmarks: ${BLUE}https://github.com/${REPO}${RESET}\n\n"
