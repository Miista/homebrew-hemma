package main

import (
	"os"

	"splitdns/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
