package dockyard

// The Dockyard release hosts trust: a tag and the SHA-256 of its dockyard.json.
// The index pins every image file by size and SHA-256, so verifying the index
// against this pin makes the whole release tamper-evident. Bump both together
// when a new images-* release is adopted.
const (
	DockyardTag         = "images-2026.10.11"
	DockyardIndexSHA256 = "211d6fbd7ca2da7737e6f0f07ff04009d3cb7ab2152eb1fde7c0106929696194"
	DockyardIndexURL    = "https://github.com/usedock/docklet/releases/download/" + DockyardTag + "/dockyard.json"
)

// ValidName reports whether s is a path-safe image name.
func ValidName(s string) bool { return nameRe.MatchString(s) }
