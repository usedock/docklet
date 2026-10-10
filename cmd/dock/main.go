// Command dock is the CLI for the Dock control plane.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/usedock/docklet/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.NewApp().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
