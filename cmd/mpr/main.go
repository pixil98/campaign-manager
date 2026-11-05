package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/pixil98/campaign-manager/cmd/mpr/command"
)

func main() {
	// Ctrl-C cancels requests in flight and a login being waited on.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := command.NewRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
