package cdpmonitor

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
)

// These bundled extensions are loaded unpacked at fixed paths without a manifest
// key. Chromium derives their IDs from the first 128 bits of SHA-256(path).
var bundledExtensionIDs = map[string]bool{
	unpackedExtensionID("/home/kernel/extensions/kernel"):     true,
	unpackedExtensionID("/home/kernel/extensions/capmonster"): true,
}

func unpackedExtensionID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return strings.Map(func(r rune) rune {
		if r <= '9' {
			return 'a' + r - '0'
		}
		return 'k' + r - 'a'
	}, hex.EncodeToString(sum[:16]))
}

func targetMetadata(info targetInfo) map[string]string {
	metadata := map[string]string{MetadataKeyTargetID: info.targetID, MetadataKeyTargetType: info.targetType}
	origin, err := url.Parse(info.url)
	if err == nil && origin.Scheme == "chrome-extension" && origin.Host != "" {
		metadata["extension_id"] = origin.Host
		metadata["kernel_extension"] = strconv.FormatBool(bundledExtensionIDs[origin.Host])
	}
	return metadata
}
