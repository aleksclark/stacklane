package main

import (
	"fmt"
	"os"

	"github.com/aleksclark/stacklane/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	fmt.Fprintf(os.Stderr, "usage: %s version\n", os.Args[0])
	os.Exit(2)
}
