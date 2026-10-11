package local

// The dock binary is cosign-signed, so what is pinned here is signed with it: the Dockyard
// release the CLI reads its index from and the sha256 of that release's dockyard.json.
// The index lists every image file with its own size and sha256, so pinning the index
// pins the images. The first release is images-2026.10.11; bump both together for a newer one.
const (
	// DockyardTag is the images-* release tag, for example "images-20260101".
	DockyardTag = "images-2026.10.11"
	// DockyardIndexSHA256 is the lowercase hex sha256 of that release's dockyard.json.
	DockyardIndexSHA256 = "211d6fbd7ca2da7737e6f0f07ff04009d3cb7ab2152eb1fde7c0106929696194"
)

// PinnedIndexURL is the index URL of the pinned release, or empty when none is pinned.
func PinnedIndexURL() string {
	if DockyardTag == "" || DockyardIndexSHA256 == "" {
		return ""
	}
	return "https://github.com/usedock/docklet/releases/download/" + DockyardTag + "/dockyard.json"
}
