package dockyard

// The Dockyard release hosts trust: a tag and the SHA-256 of its dockyard.json.
// The index pins every image file by size and SHA-256, so verifying the index
// against this pin makes the whole release tamper-evident. Bump both together
// when a new images-* release is adopted.
const (
	DockyardTag         = "images-2026.10.11.2"
	DockyardIndexSHA256 = "239277107001287d16619a378eff5fd73cb292c60d0b0e360fd2cf3435ece98b"
	DockyardIndexURL    = "https://github.com/usedock/docklet/releases/download/" + DockyardTag + "/dockyard.json"
)

// ValidName reports whether s is a path-safe image name.
func ValidName(s string) bool { return nameRe.MatchString(s) }
