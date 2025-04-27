package main

import (
	"go-civitai-download/cmd/civitai-downloader/cmd"
	"go-civitai-download/internal/api"
)

func main() {
	// Ensure all API log file buffers are flushed and files closed on exit
	defer api.CloseAllLoggingTransports()

	// Execute the root command (defined in cmd/root.go)
	cmd.Execute()
}
