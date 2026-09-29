#!/usr/bin/env bash
# ==============================================================================
# 絆 KIZUNA - Installer for Linux & macOS
# Repository: https://github.com/osuki-dev/kizuna
# ==============================================================================
set -euo pipefail

GITHUB_REPO="osuki-dev/kizuna"
INSTALL_DIR="${KIZUNA_DIR:-/usr/local/bin}"
ALT_INSTALL_DIR="${HOME}/.local/bin"
BINARY_NAME="kizuna"

# Terminal Colors & Styling
BOLD="$(tput bold 2>/dev/null || echo '')"
RESET="$(tput sgr0 2>/dev/null || echo '')"
CYAN="$(tput setaf 6 2>/dev/null || echo '')"
GREEN="$(tput setaf 2 2>/dev/null || echo '')"
YELLOW="$(tput setaf 3 2>/dev/null || echo '')"
RED="$(tput setaf 1 2>/dev/null || echo '')"
PURPLE="$(tput setaf 5 2>/dev/null || echo '')"

info() {
    printf "${CYAN}==>${RESET} ${BOLD}%s${RESET}\n" "$*"
}

success() {
    printf "${GREEN}✓${RESET} %s\n" "$*"
}

warn() {
    printf "${YELLOW}⚠${RESET} %s\n" "$*"
}

error() {
    printf "${RED}✖ Error:${RESET} %s\n" "$*" >&2
    exit 1
}

# 1. Print Banner
printf "${PURPLE}"
cat << 'EOF'
 _  ___                     
| |/ (_)____  _ _ _  __ _   
| ' <| |_ / || | ' \/ _` |  
|_|\_\_/__|\_,_|_||_\__,_|  
 絆 KIZUNA - Installer
EOF
printf "${RESET}\n"

# 2. Detect Operating System & Architecture
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m | tr '[:upper:]' '[:lower:]')"

case "${OS}" in
    linux*)  OS="linux" ;;
    darwin*) OS="darwin" ;;
    *) error "Unsupported operating system: ${OS}. Kizuna supports Linux and macOS." ;;
esac

case "${ARCH}" in
    x86_64|amd64)   ARCH="amd64" ;;
    arm64|aarch64)  ARCH="arm64" ;;
    *) error "Unsupported architecture: ${ARCH}. Kizuna supports amd64 and arm64." ;;
esac

info "Detected platform: ${OS}/${ARCH}"

# 3. Determine Version
VERSION="${KIZUNA_VERSION:-}"
if [ -z "${VERSION}" ]; then
    info "Fetching latest release tag from GitHub..."
    LATEST_JSON="$(curl -fsSL -H "Accept: application/vnd.github.v3+json" \
        "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" 2>/dev/null || true)"
    if [ -n "${LATEST_JSON}" ]; then
        VERSION="$(echo "${LATEST_JSON}" | grep -o '"tag_name": *"[^"]*"' | head -n1 | cut -d'"' -f4)"
    fi
    if [ -z "${VERSION}" ]; then
        VERSION="v0.2.1"
        warn "Could not fetch latest release from GitHub API, falling back to default ${VERSION}"
    fi
fi
info "Target version: ${VERSION}"

# 4. Prepare Download URL & Temporary Workspace
TMP_DIR="$(mktemp -d)"
cleanup() {
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT INT TERM

# Primary & fallback archive name patterns
ARCHIVE_NAME="kizuna_${VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"
CHECKSUMS_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/checksums.txt"

info "Downloading ${ARCHIVE_NAME}..."
HTTP_STATUS="$(curl -fsSL -w "%{http_code}" -o "${TMP_DIR}/${ARCHIVE_NAME}" "${DOWNLOAD_URL}" || echo "000")"
if [ "${HTTP_STATUS}" != "200" ] && [ ! -s "${TMP_DIR}/${ARCHIVE_NAME}" ]; then
    # Try alternate naming pattern: kizuna-<os>-<arch>
    ARCHIVE_NAME="kizuna-${OS}-${ARCH}"
    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${ARCHIVE_NAME}"
    info "Trying direct binary: ${DOWNLOAD_URL}..."
    curl -fsSL -o "${TMP_DIR}/${ARCHIVE_NAME}" "${DOWNLOAD_URL}" || \
        error "Failed to download release from ${DOWNLOAD_URL}"
fi

# 5. SHA256 Checksum Verification
info "Verifying cryptographic SHA256 checksum..."
if curl -fsSL -o "${TMP_DIR}/checksums.txt" "${CHECKSUMS_URL}" 2>/dev/null && [ -s "${TMP_DIR}/checksums.txt" ]; then
    EXPECTED_HASH="$(grep "${ARCHIVE_NAME}" "${TMP_DIR}/checksums.txt" | awk '{print $1}' | head -n1 || true)"
    if [ -n "${EXPECTED_HASH}" ]; then
        if command -v sha256sum >/dev/null 2>&1; then
            ACTUAL_HASH="$(sha256sum "${TMP_DIR}/${ARCHIVE_NAME}" | awk '{print $1}')"
        elif command -v shasum >/dev/null 2>&1; then
            ACTUAL_HASH="$(shasum -a 256 "${TMP_DIR}/${ARCHIVE_NAME}" | awk '{print $1}')"
        else
            warn "sha256sum/shasum not available, skipping cryptographic hash check."
            ACTUAL_HASH=""
        fi

        if [ -n "${ACTUAL_HASH}" ]; then
            if [ "${ACTUAL_HASH}" != "${EXPECTED_HASH}" ]; then
                error "SHA256 checksum mismatch! Expected ${EXPECTED_HASH}, but got ${ACTUAL_HASH}."
            fi
            success "SHA256 checksum verified: ${ACTUAL_HASH:0:16}..."
        fi
    else
        warn "Archive name not found in checksums.txt, continuing."
    fi
else
    warn "No checksums.txt found in release, proceeding without hash verification."
fi

# 6. Extract Binary
if [[ "${ARCHIVE_NAME}" == *.tar.gz ]]; then
    tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "${TMP_DIR}"
fi

if [ -f "${TMP_DIR}/${BINARY_NAME}" ]; then
    SRC_BIN="${TMP_DIR}/${BINARY_NAME}"
elif [ -f "${TMP_DIR}/${ARCHIVE_NAME}" ]; then
    SRC_BIN="${TMP_DIR}/${ARCHIVE_NAME}"
    chmod +x "${SRC_BIN}"
else
    # Find any executable named kizuna
    SRC_BIN="$(find "${TMP_DIR}" -type f -name "${BINARY_NAME}*" | head -n1 || true)"
fi

if [ -z "${SRC_BIN}" ] || [ ! -f "${SRC_BIN}" ]; then
    error "Could not find binary executable in downloaded release archive."
fi
chmod +x "${SRC_BIN}"

# 7. Install to Target Directory
TARGET_PATH="${INSTALL_DIR}/${BINARY_NAME}"
info "Installing to ${TARGET_PATH}..."

USE_SUDO=""
if [ ! -w "${INSTALL_DIR}" ]; then
    if command -v sudo >/dev/null 2>&1 && [ -t 0 ]; then
        USE_SUDO="sudo"
    else
        TARGET_PATH="${ALT_INSTALL_DIR}/${BINARY_NAME}"
        mkdir -p "${ALT_INSTALL_DIR}"
        info "Falling back to user bin directory: ${TARGET_PATH}"
    fi
fi

${USE_SUDO} mkdir -p "$(dirname "${TARGET_PATH}")"
${USE_SUDO} cp -f "${SRC_BIN}" "${TARGET_PATH}"
${USE_SUDO} chmod +x "${TARGET_PATH}"

# macOS Security & Code Signing Handling:
# On Apple Silicon (darwin/arm64), binaries cross-compiled on Linux or downloaded from the internet
# get killed with SIGKILL (Killed: 9) unless quarantine flags are cleared and an ad-hoc signature is applied.
if [ "${OS}" = "darwin" ]; then
    info "Configuring macOS security attributes and ad-hoc code signature..."
    ${USE_SUDO} xattr -cr "${TARGET_PATH}" 2>/dev/null || true
    if command -v codesign >/dev/null 2>&1; then
        ${USE_SUDO} codesign --force --deep -s - "${TARGET_PATH}" 2>/dev/null || true
    fi
fi

success "Successfully installed ${TARGET_PATH}"
"${TARGET_PATH}" --version || true

# 8. Check PATH
case ":${PATH}:" in
    *:"$(dirname "${TARGET_PATH}")":*) ;;
    *)
        warn "$(dirname "${TARGET_PATH}") is not in your \$PATH!"
        echo "  Add it to your profile: export PATH=\"$(dirname "${TARGET_PATH}"):\$PATH\""
        ;;
esac

# 9. Prompt for System Service Installation
WANT_SERVICE="${INSTALL_SERVICE:-}"
if [ -z "${WANT_SERVICE}" ] && [ -t 0 ]; then
    printf "\n"
    read -r -p "${CYAN}?${RESET} ${BOLD}Do you want to install and start Kizuna as a background system service? [y/N]: ${RESET}" REPLY || REPLY=""
    case "${REPLY}" in
        [yY]|[yY][eE][sS]) WANT_SERVICE="yes" ;;
        *) WANT_SERVICE="no" ;;
    esac
fi

if [ "${WANT_SERVICE}" = "yes" ] || [ "${WANT_SERVICE}" = "true" ]; then
    info "Installing Kizuna background service..."
    if [ -n "${USE_SUDO}" ]; then
        ${USE_SUDO} "${TARGET_PATH}" service install --system
    else
        "${TARGET_PATH}" service install --user
    fi
    success "Kizuna background service registered and started!"
else
    echo "  ${BOLD}Tip:${RESET} You can install the background daemon anytime by running '${BINARY_NAME} service install'."
fi

printf "\n${GREEN}${BOLD}★ Kizuna is ready!${RESET}\n"
echo "  Run '${BINARY_NAME} --help' to explore commands"
echo "  Run '${BINARY_NAME} init' in any project directory to create a configuration"
echo ""
