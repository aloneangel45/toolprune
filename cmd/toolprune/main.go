package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if errors.Is(err, exec.ErrNotFound) {
			os.Exit(127)
		}
		os.Exit(1)
	}
}
