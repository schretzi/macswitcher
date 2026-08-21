// Command gendocs generates Markdown documentation for the macswitcher CLI
// command tree into the docs/ directory.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra/doc"

	"macswitcher/internal/app"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	const outDir = "docs"
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	return doc.GenMarkdownTree(app.Root(), outDir)
}
