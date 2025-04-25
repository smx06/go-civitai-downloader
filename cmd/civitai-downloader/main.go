package main

import (
	"go-civitai-download/cmd/civitai-downloader/cmd"
	"go-civitai-download/internal/api"
)

func main() {
	// Ensure API log file is closed on exit
	defer api.CleanupApiLog()

	// Execute the root command (defined in cmd/root.go)
	cmd.Execute()
}
