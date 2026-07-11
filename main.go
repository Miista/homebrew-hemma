package main

import (
	"os"

	"hemma/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
