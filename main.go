package main

import (
	"os"

	"mirage/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
