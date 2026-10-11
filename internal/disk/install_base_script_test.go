package disk

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runInstallBase runs scripts/install-base.sh against an image dir.
func runInstallBase(t *testing.T, imageDir string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install-base.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "OUT_DIR="+imageDir)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestInstallBaseScriptAddsBasesAndKeepsOldOnes(t *testing.T) {
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	pointer := filepath.Join(images, "base.ext4")
	// A legacy host: a plain base.ext4 some generations were built against.
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatal(err)
	}
	writeImage(t, pointer, 2*BlockSize)
	legacy, err := Fingerprint(pointer)
	if err != nil {
		t.Fatal(err)
	}
	built := filepath.Join(dir, "built.ext4")
	writeImage(t, built, 2*BlockSize)
	writeAt(t, built, 5, []byte("rebuilt"))
	want, err := Fingerprint(built)
	if err != nil {
		t.Fatal(err)
	}

	fp, err := runInstallBase(t, images, built)
	if err != nil {
		t.Fatalf("install-base: %v", err)
	}
	// The script and the docklet must agree on identity, or restores miss.
	if fp != want {
		t.Fatalf("script fingerprint = %q, docklet computes %q", fp, want)
	}
	if _, cur, err := CurrentBase(pointer); err != nil || cur != want {
		t.Fatalf("current = %q, %v; want %q", cur, err, want)
	}
	if path, err := FindBase(pointer, legacy); err != nil || path != BasePath(pointer, legacy) {
		t.Fatalf("legacy base = %q, %v; want it preserved under bases/", path, err)
	}
	listed, _ := ListBases(pointer)
	if len(listed) != 2 {
		t.Fatalf("bases = %v, want the legacy and the rebuilt one", listed)
	}
	// No temp files are left behind in the content-addressed store.
	entries, _ := os.ReadDir(filepath.Join(images, BasesDir))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("leftover temp %s", e.Name())
		}
	}
	// Re-pointing an installed base by fingerprint copies nothing.
	if fp, err := runInstallBase(t, images, BasePath(pointer, legacy), legacy); err != nil || fp != legacy {
		t.Fatalf("repoint = %q, %v", fp, err)
	}
	if _, cur, _ := CurrentBase(pointer); cur != legacy {
		t.Fatalf("current = %q, want %q", cur, legacy)
	}
}

func TestInstallBaseScriptRefusesAChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	src := filepath.Join(dir, "image.ext4")
	writeImage(t, src, BlockSize)
	if _, err := runInstallBase(t, images, src, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a checksum mismatch must fail the install")
	}
	if _, err := os.Lstat(filepath.Join(images, "base.ext4")); !os.IsNotExist(err) {
		t.Fatalf("pointer must not be created on refusal, lstat err = %v", err)
	}
	if listed, _ := ListBases(filepath.Join(images, "base.ext4")); len(listed) != 0 {
		t.Fatalf("bases = %v, want none", listed)
	}
}

func TestInstallBaseScriptReplacesACorruptInstalledBase(t *testing.T) {
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	pointer := filepath.Join(images, "base.ext4")
	src := filepath.Join(dir, "image.ext4")
	writeImage(t, src, 2*BlockSize)
	fp, err := runInstallBase(t, images, src)
	if err != nil {
		t.Fatalf("install-base: %v", err)
	}
	// A torn write: same size, wrong bytes, under the trusted name.
	writeAt(t, BasePath(pointer, fp), BlockSize, []byte("torn"))
	if _, err := runInstallBase(t, images, BasePath(pointer, fp), fp); err == nil {
		t.Fatal("making a corrupt base current must fail")
	}
	if got, err := runInstallBase(t, images, src, fp); err != nil || got != fp {
		t.Fatalf("reinstall = %q, %v", got, err)
	}
	if !bytes.Equal(readAll(t, BasePath(pointer, fp)), readAll(t, src)) {
		t.Fatal("corrupt base was not replaced by the good copy")
	}
	entries, _ := os.ReadDir(filepath.Join(images, BasesDir))
	var quarantined int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".corrupt-"+fp) {
			quarantined++
		}
	}
	if quarantined != 1 {
		t.Fatalf("quarantined dirs = %d, want the corrupt copy moved aside once", quarantined)
	}
}

func TestInstallBaseScriptRequiresAFullChecksum(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "image.ext4")
	writeImage(t, src, BlockSize)
	if _, err := runInstallBase(t, filepath.Join(dir, "images"), src, "abc"); err == nil {
		t.Fatal("a truncated checksum must be refused")
	}
}

// A live microVM is unavailable in CI, so pin the mechanism that makes the
// guest filesystem follow Grow: the image's root fstab entry must carry
// x-systemd.growfs for the device the kernel boots from.
func TestBuildImageGrowsRootFilesystemAtBoot(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "build-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "/dev/vda / ext4 defaults,x-systemd.growfs") {
		t.Fatal("build-image.sh must write a root fstab entry with x-systemd.growfs")
	}
	host, err := os.ReadFile(filepath.Join("..", "host", "host.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(host), "root=/dev/vda ") {
		t.Fatal("kernel root device changed: update the fstab entry in build-image.sh")
	}
}

// The scripts are stored without the executable bit, so install-kernel.sh must run its sibling
// through bash: calling it directly fails with "Permission denied" on a fresh checkout.
func TestInstallKernelRunsExtractVmlinuxThroughBash(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install-kernel.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(script), "\n") {
		if strings.Contains(line, "extract-vmlinux.sh") && !strings.HasPrefix(strings.TrimSpace(line), "#") && !strings.HasPrefix(line, "bash ") {
			t.Fatalf("install-kernel.sh must run extract-vmlinux.sh with bash: %q", line)
		}
	}
}
