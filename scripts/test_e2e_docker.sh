#!/usr/bin/env bash
set -euo pipefail

echo "============================================================"
echo "  Kizuna End-to-End Docker Verification Suite"
echo "============================================================"

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${PROJECT_DIR}"

CONTAINER_NAME="kizuna-e2e-suite-node"
TEST_PORT="19808"

# 1. Build host binary
echo "==> Step 1: Building latest kizuna binary..."
make build
KIZUNA="${PROJECT_DIR}/bin/kizuna"

# Cleanup trap
cleanup() {
    echo "==> Cleaning up test resources..."
    docker rm -f "${CONTAINER_NAME}" 2>/dev/null || true
    "${KIZUNA}" node rm "e2e-test-node" 2>/dev/null || true
    echo "==> Cleaned up successfully."
}
trap cleanup EXIT

# 2. Test service installation inside container
echo "==> Step 2: Testing service install inside clean Linux container..."
docker run --rm -v "${KIZUNA}:/usr/local/bin/kizuna:ro" ubuntu:22.04 bash -c '
    kizuna service install
    if [ -f /etc/init.d/kizuna-server ] || [ -f /etc/systemd/system/kizuna-server.service ]; then
        echo "✓ System service unit registered successfully!"
    else
        echo "✗ Failed to register system service unit!"
        exit 1
    fi
'

# 3. Launch live node container
echo "==> Step 3: Launching live node container with 'kizuna service run'..."
docker run -d --name "${CONTAINER_NAME}" \
    -v "${KIZUNA}:/usr/local/bin/kizuna:ro" \
    -p "${TEST_PORT}:19800" \
    ubuntu:22.04 kizuna service run

sleep 2

# 4. Extract pairing PIN from container logs
echo "==> Step 4: Extracting pairing PIN..."
PIN=$(docker logs "${CONTAINER_NAME}" 2>&1 | grep -oE '[0-9]{6}' | head -n 1)
echo "    Found PIN: ${PIN}"

# 5. Pair node from host
echo "==> Step 5: Pairing node from host CLI..."
"${KIZUNA}" node add "127.0.0.1:${TEST_PORT}" --pin "${PIN}" --name "e2e-test-node"

# 6. Verify online status
echo "==> Step 6: Verifying node status in mesh list..."
STATUS_OUT=$("${KIZUNA}" node list)
echo "${STATUS_OUT}"
if echo "${STATUS_OUT}" | grep -q "Online"; then
    echo "✓ Node is ONLINE and actively responding to telemetry!"
else
    echo "✗ Node did not report Online status!"
    exit 1
fi

# 7. Test container restart & auto-reconnect
echo "==> Step 7: Restarting node container (simulating server reboot)..."
docker restart "${CONTAINER_NAME}"
sleep 2

echo "==> Step 8: Verifying node is still ONLINE after reboot..."
RESTART_OUT=$("${KIZUNA}" node list)
echo "${RESTART_OUT}"
if echo "${RESTART_OUT}" | grep -q "Online"; then
    echo "✓ Node successfully reconnected and reports Online after reboot!"
else
    echo "✗ Node failed to report Online after reboot!"
    exit 1
fi

echo "============================================================"
echo "  ✓ ALL E2E DOCKER TESTS PASSED SUCCESSFULLY!"
echo "============================================================"
