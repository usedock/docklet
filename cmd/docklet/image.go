package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/usedock/docklet/internal/host"
)

// runImage implements `docklet image pull <name>` and `docklet image ls`,
// returning the process exit code.
func runImage(args []string, stdout, stderr io.Writer, p *host.Puller) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: docklet image pull <name> | docklet image ls")
		return 2
	}
	switch args[0] {
	case "ls":
		fmt.Fprintf(stdout, "%-20s %-20s %s\n", "NAME", "RELEASE", "FINGERPRINT")
		for _, img := range host.ListInstalled(p.Dir) {
			fmt.Fprintf(stdout, "%-20s %-20s %s\n", img.Name, img.Version, img.Fingerprint)
		}
		return 0
	case "pull":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: docklet image pull <name>...")
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		code := 0
		for _, name := range args[1:] {
			img, err := p.Pull(ctx, name)
			if err != nil {
				msg := err.Error()
				if errors.Is(err, host.ErrImageNotInCatalog) {
					msg += " (docklet image ls shows what is installed)"
				}
				fmt.Fprintf(stderr, "docklet: pull %s: %s\n", name, msg)
				code = 1
				continue
			}
			fmt.Fprintf(stdout, "pulled %s (%s, %s)\n", img.Name, img.Version, img.Fingerprint)
		}
		return code
	}
	fmt.Fprintf(stderr, "docklet: unknown image command %q\n", args[0])
	return 2
}

// imagePuller builds the puller for an `image` command. `image ls` never
// downloads, so it ignores the Dockyard override pair (even half-set).
func imagePuller(args []string, imageDir string) (*host.Puller, error) {
	if len(args) > 0 && args[0] == "ls" {
		return host.NewPuller(imageDir), nil
	}
	return newPuller(imageDir)
}

// newPuller pins the release this binary trusts; DOCKLET_DOCKYARD_INDEX_URL and
// DOCKLET_DOCKYARD_INDEX_SHA256 together move a host to another release.
func newPuller(imageDir string) (*host.Puller, error) {
	p := host.NewPuller(imageDir)
	url, sum := os.Getenv("DOCKLET_DOCKYARD_INDEX_URL"), os.Getenv("DOCKLET_DOCKYARD_INDEX_SHA256")
	if (url == "") != (sum == "") {
		return nil, errors.New("set DOCKLET_DOCKYARD_INDEX_URL and DOCKLET_DOCKYARD_INDEX_SHA256 together")
	}
	if url != "" {
		p.IndexURL, p.IndexSHA256 = url, sum
	}
	return p, nil
}
