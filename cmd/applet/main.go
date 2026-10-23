// Command applet creates, deploys, and manages Applet apps.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/applet-one/cli/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app, err := cli.NewApp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "applet: %s\n", err)
		os.Exit(1)
	}
	code := cli.Execute(ctx, app, os.Args[1:])
	stop()
	os.Exit(code)
}
