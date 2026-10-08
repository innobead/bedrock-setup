// Command bedrock-admin sets up and runs Amazon Bedrock access with per-user monthly limits.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/SUSE/high-impact-ai-initiative/internal/admincli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := admincli.New(version).Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
