package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	// Use correct relative paths for internal packages

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func init() {
	// Assumes rootCmd is defined in root.go within the same package
	rootCmd.AddCommand(cleanCmd)
}

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove temporary (.tmp) files from the download directory",
	Long:  `Recursively scans the configured SavePath and removes any files ending with the .tmp extension.`,
	Run:   runClean,
}

func runClean(cmd *cobra.Command, args []string) {
	// Access the globally loaded config from root.go's PersistentPreRunE
	// No need to call config.LoadConfig() here again.
	cfg := globalConfig // Use the globalConfig variable
	savePath := cfg.SavePath

	// Check if SavePath is actually set after loading and overrides
	if savePath == "" {
		// Attempt to construct default if SavePath is empty but DatabasePath isn't (less common)
		if cfg.DatabasePath != "" {
			savePath = filepath.Dir(cfg.DatabasePath) // Infer SavePath from DB path's directory
			log.Warnf("SavePath is empty, inferring base directory from DatabasePath: %s", savePath)
		} else {
			log.Error("SavePath is not configured (and cannot be inferred from DatabasePath). Cannot determine where to clean.")
			os.Exit(1)
		}
	}

	// Ensure the path exists and is a directory before walking
	info, err := os.Stat(savePath)
	if os.IsNotExist(err) {
		log.Errorf("SavePath directory does not exist: %s", savePath)
		os.Exit(1)
	}
	if err != nil {
		log.Errorf("Error accessing SavePath %q: %v", savePath, err)
		os.Exit(1)
	}
	if !info.IsDir() {
		log.Errorf("SavePath is not a directory: %s", savePath)
		os.Exit(1)
	}

	log.Infof("Scanning for .tmp files in %s...", savePath)

	var filesRemoved int64 // Use int64 for counter
	var filesFailed int64

	walkErr := filepath.Walk(savePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Log errors accessing specific files/subdirs but continue if possible
			log.Warnf("Error accessing path %q during scan: %v", path, err)
			// Decide if the error is critical enough to stop the whole walk
			// For now, let's return nil to attempt to continue with other parts.
			// If you want to stop on any error, return err here.
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".tmp") {
			log.Debugf("Found temporary file: %s", path)
			err := os.Remove(path)
			if err != nil {
				// Check if it's a 'not exist' error - could happen in race conditions
				if os.IsNotExist(err) {
					log.Warnf("Attempted to remove %q, but it was already gone.", path)
				} else {
					log.Errorf("Failed to remove temporary file %q: %v", path, err)
					filesFailed++
				}
			} else {
				log.Infof("Removed temporary file: %s", path)
				filesRemoved++
			}
		}
		return nil // Continue walking
	})

	// Check the error returned by filepath.Walk itself (e.g., initial dir access error)
	if walkErr != nil {
		log.Errorf("Error during directory walk of %q: %v", savePath, walkErr)
		// Don't exit here necessarily, maybe some files were cleaned
	}

	summary := fmt.Sprintf("Clean complete. Removed %d temporary file(s)", filesRemoved)
	if filesFailed > 0 {
		summary += fmt.Sprintf(", failed to remove %d file(s)", filesFailed)
	}
	log.Info(summary)

	if filesFailed > 0 || walkErr != nil {
		// Indicate potential issues with exit code if there were failures
		os.Exit(1)
	}
}
