// Command dock is the CLI for the Dock control plane.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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

// shutdownSignals end a command the way Ctrl-C does, so deferred cleanup (temp files of a
// push or pull) runs on a service stop or `kill` too.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, shutdownSignals...)
}

func main() {
	if printVersion(os.Stdout, os.Args[1:]) {
		return
	}
	ctx, stop := signalContext(context.Background())
	code := cli.NewApp().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
