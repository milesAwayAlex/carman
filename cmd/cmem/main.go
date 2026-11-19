// Command cmem is the carman memory: the service and its command-line client.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"carman/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.RunWith(ctx, os.Args[1:], os.Getenv, os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}
