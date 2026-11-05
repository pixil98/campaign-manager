package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/pixil98/campaign-manager/cmd/campaign-manager/command"
	"github.com/pixil98/go-service"
)

func main() {
	slog.Info("creating application")

	app, err := service.NewApp(&command.Config{}, command.BuildWorkers)
	if err != nil {
		slog.Error("creating application", "error", err)
		os.Exit(1)
	}

	err = app.Run(context.Background())
	if err != nil {
		slog.Error("running application", "error", err)
		os.Exit(1)
	}

	slog.Info("exiting application")
}
