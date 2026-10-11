package local

// The dock binary is cosign-signed, so what is pinned here is signed with it: the Dockyard
// release the CLI reads its index from and the sha256 of that release's dockyard.json.
// The index lists every image file with its own size and sha256, so pinning the index
// pins the images. Both stay empty until the first images-* release is cut.
const (
	// DockyardTag is the images-* release tag, for example "images-20260101".
	DockyardTag = ""
	// DockyardIndexSHA256 is the lowercase hex sha256 of that release's dockyard.json.
	DockyardIndexSHA256 = ""
)

// PinnedIndexURL is the index URL of the pinned release, or empty when none is pinned.
func PinnedIndexURL() string {
	if DockyardTag == "" || DockyardIndexSHA256 == "" {
		return ""
	}
	return "https://github.com/usedock/docklet/releases/download/" + DockyardTag + "/dockyard.json"
}
