package dockyard

import (
	"strings"
	"testing"
)

func TestThePinLooksLikeARelease(t *testing.T) {
	if !strings.HasPrefix(DockyardTag, "images-") {
		t.Fatalf("DockyardTag %q must be an images-* tag", DockyardTag)
	}
	if len(DockyardIndexSHA256) != 64 || strings.ToLower(DockyardIndexSHA256) != DockyardIndexSHA256 {
		t.Fatalf("DockyardIndexSHA256 %q must be 64 lowercase hex chars", DockyardIndexSHA256)
	}
	want := "https://github.com/usedock/docklet/releases/download/" + DockyardTag + "/dockyard.json"
	if DockyardIndexURL != want {
		t.Fatalf("pinned URL %q, want %q", DockyardIndexURL, want)
	}
}
