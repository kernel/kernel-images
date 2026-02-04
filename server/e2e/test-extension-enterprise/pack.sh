#!/bin/bash
# Pack the test extension into a .crx file
# Usage: ./pack.sh
#
# Requires: google-chrome (or chromium), openssl, python3
#
# This script:
# 1. Packs the extension using Chrome's built-in packer
# 2. Extracts and displays the extension ID from the private key
#
# The extension ID is derived from the public key, so as long as you use
# the same private_key.pem, you'll get the same extension ID.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Find Chrome binary
CHROME=""
for bin in google-chrome chromium chromium-browser; do
    if command -v "$bin" &> /dev/null; then
        CHROME="$bin"
        break
    fi
done

if [ -z "$CHROME" ]; then
    echo "Error: Chrome/Chromium not found"
    exit 1
fi

# Check for private key
if [ ! -f "private_key.pem" ]; then
    echo "Generating new private key..."
    openssl genrsa -out private_key.pem 2048
fi

# Chrome won't pack if the key is inside the extension directory
mv private_key.pem /tmp/ext_key.pem
trap 'mv /tmp/ext_key.pem private_key.pem 2>/dev/null || true' EXIT

# Pack the extension (Chrome creates .crx in parent directory)
echo "Packing extension..."
"$CHROME" --pack-extension="$SCRIPT_DIR" --pack-extension-key=/tmp/ext_key.pem --no-sandbox 2>&1 || true

# Move the .crx file into place
PARENT_DIR="$(dirname "$SCRIPT_DIR")"
CRX_NAME="$(basename "$SCRIPT_DIR").crx"
if [ -f "$PARENT_DIR/$CRX_NAME" ]; then
    mv "$PARENT_DIR/$CRX_NAME" extension.crx
    echo "Created extension.crx"
else
    echo "Error: Chrome did not create .crx file"
    exit 1
fi

# Restore key before computing ID
mv /tmp/ext_key.pem private_key.pem
trap - EXIT

# Extract extension ID from the public key
EXT_ID=$(python3 -c "
import hashlib
import subprocess
result = subprocess.run(
    ['openssl', 'rsa', '-in', 'private_key.pem', '-pubout', '-outform', 'DER'],
    stdout=subprocess.PIPE, stderr=subprocess.PIPE
)
sha = hashlib.sha256(result.stdout).digest()
print(''.join(chr(ord('a') + (b >> 4)) + chr(ord('a') + (b & 0xf)) for b in sha[:16]))
")

echo "Extension ID: $EXT_ID"
echo ""
echo "Make sure update.xml contains this appid:"
echo "  <app appid='$EXT_ID'>"
