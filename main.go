package main

import (
	"os"

	"sd/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
