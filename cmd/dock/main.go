// Command dock is the CLI for the Dock control plane.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/usedock/docklet/internal/cli"
)

// version is set at release build time with -ldflags "-X main.version=<tag>".
var version = "dev"

// printVersion handles `dock version` and `dock --version`; it reports whether it did.
func printVersion(w io.Writer, args []string) bool {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(w, version)
		return true
	}
	return false
}

func main() {
	if printVersion(os.Stdout, os.Args[1:]) {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.NewApp().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
