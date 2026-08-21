package main

import (
	"fmt"
	"os"

	"macswitcher/internal/app"
)

func main() {
	if err := app.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
