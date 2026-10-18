package main

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/prepaser/mytracker/internal/app"
)

//go:embed VERSION
var version string

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && (args[0] == "-version" || args[0] == "--version") {
		fmt.Printf("mytracker %s\n", strings.TrimSpace(version))
		return 0
	}
	return app.Run(args)
}
