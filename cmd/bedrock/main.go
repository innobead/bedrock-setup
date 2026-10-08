// Command bedrock sets up and checks the AWS profile of a user's personal Amazon Bedrock role.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/SUSE/high-impact-ai-initiative/internal/usercli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := usercli.New(version).Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
