package downloader

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go-civitai-download/internal/helpers"
	"go-civitai-download/internal/models"

	log "github.com/sirupsen/logrus"
)

// Custom Downloader Errors
var (
	ErrHashMismatch = errors.New("downloaded file hash mismatch")
	ErrHttpStatus   = errors.New("unexpected HTTP status code")
	ErrFileSystem   = errors.New("filesystem error") // Covers create, remove, rename
	ErrHttpRequest  = errors.New("HTTP request creation/execution error")
)

// Downloader handles downloading files with progress and hash checks.
type Downloader struct {
	client *http.Client
	apiKey string // Add field to store API key
}

// NewDownloader creates a new Downloader instance.
func NewDownloader(client *http.Client, apiKey string) *Downloader {
	if client == nil {
		// Provide a default client if none is passed
		client = &http.Client{
			Timeout: 15 * time.Minute,
		}
	}
	return &Downloader{
		client: client,
		apiKey: apiKey, // Store the API key
	}
}

// DownloadFile downloads a file from the specified URL to the target filepath.
// It checks for existing files, verifies hashes, and attempts to use the
// Content-Disposition header for the filename.
// It also now accepts a modelVersionID to prepend to the final filename.
// Returns the final filepath used (or empty string on failure) and an error if one occurred.
func (d *Downloader) DownloadFile(targetFilepath string, url string, hashes models.Hashes, modelVersionID int) (string, error) {
	finalFilepath := targetFilepath // Initialize final path

	// Check if file exists and hash matches
	if _, err := os.Stat(targetFilepath); err == nil {
		if helpers.CheckHash(targetFilepath, hashes) {
			log.Infof("File %s is up to date, skipping download.", targetFilepath)
			return targetFilepath, nil // Success, no error
		} else {
			log.Warnf("File %s exists but hash mismatch, redownloading.", targetFilepath)
			err = os.Remove(targetFilepath)
			if err != nil {
				log.WithError(err).Errorf("Error removing existing file %s", targetFilepath)
				// Wrap the error
				return "", fmt.Errorf("%w: removing existing file %s: %v", ErrFileSystem, targetFilepath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		// Error stating the file, other than not existing
		log.WithError(err).Errorf("Error checking status of target file %s", targetFilepath)
		return "", fmt.Errorf("%w: checking target file %s: %v", ErrFileSystem, targetFilepath, err)
	}

	// Ensure target directory exists before creating temp file
	targetDir := filepath.Dir(targetFilepath)
	if !helpers.CheckAndMakeDir(targetDir) {
		return "", fmt.Errorf("%w: failed to create target directory %s", ErrFileSystem, targetDir)
	}

	// Create a temporary file in the target directory
	baseName := filepath.Base(targetFilepath)
	tempFile, err := os.CreateTemp(targetDir, baseName+".*.tmp") // Use targetDir here
	if err != nil {
		return "", fmt.Errorf("%w: creating temporary file %s: %w", ErrFileSystem, targetFilepath, err)
	}
	// Use a flag to track if we should remove the temp file on error exit
	shouldCleanupTemp := true
	defer func() {
		if err := tempFile.Close(); err != nil {
			log.WithError(err).Warnf("Error closing temp file %s during defer", tempFile.Name())
		}
		if shouldCleanupTemp {
			log.Debugf("Cleaning up temporary file: %s", tempFile.Name())
			if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
				// Log the cleanup error, but don't propagate it as the primary function error
				log.WithError(removeErr).Warnf("Failed to remove temporary file %s during cleanup", tempFile.Name())
			}
		}
	}()

	log.Infof("Attempting to download from URL: %s", url)

	// Create request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("%w: creating download request for %s: %w", ErrHttpRequest, url, err)
	}

	// Add authentication header if API key is present
	log.Debugf("Downloader stored API Key: %s", d.apiKey) // Added Debug Log
	if d.apiKey != "" {
		log.Debug("Adding Authorization header to download request.") // Added Debug Log
		req.Header.Set("Authorization", "Bearer "+d.apiKey)
	} else {
		log.Debug("No API Key found, skipping Authorization header for download.") // Added Debug Log
	}

	resp, err := d.client.Do(req)
	if err != nil {
		log.WithError(err).Errorf("Error performing download request from %s", url)
		return "", fmt.Errorf("%w: performing request for %s: %v", ErrHttpRequest, url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Errorf("Error downloading file: Received status code %d from %s", resp.StatusCode, url)
		return "", fmt.Errorf("%w: received status %d from %s", ErrHttpStatus, resp.StatusCode, url)
	}

	// --- Filename Handling from Content-Disposition ---
	// Recalculate finalFilepath based on header
	contentDisposition := resp.Header.Get("Content-Disposition")
	if contentDisposition != "" {
		_, params, err := mime.ParseMediaType(contentDisposition)
		if err == nil && params["filename"] != "" {
			apiFilename := params["filename"]
			dir := filepath.Dir(targetFilepath)             // Use original target dir
			finalFilepath = filepath.Join(dir, apiFilename) // Update finalFilepath
			log.Infof("Using filename from Content-Disposition: %s -> %s", apiFilename, finalFilepath)

			// Check if a file with the API-provided name *already* exists and matches the hash
			if finalFilepath != targetFilepath { // Only check if filename differs
				if _, err := os.Stat(finalFilepath); err == nil {
					if helpers.CheckHash(finalFilepath, hashes) {
						log.Infof("Correct file %s already exists (based on API filename), skipping download.", finalFilepath)
						shouldCleanupTemp = true  // Still remove the new temp file we created
						return finalFilepath, nil // Return success
					} else {
						log.Warnf("File %s exists (from API filename) but hash mismatch, will overwrite.", finalFilepath)
						// No need to remove here, Rename will overwrite
					}
				} else if !os.IsNotExist(err) {
					// Error stating the potentially renamed file
					log.WithError(err).Errorf("Error checking status of potential target file %s", finalFilepath)
					return "", fmt.Errorf("%w: checking potential target file %s: %v", ErrFileSystem, finalFilepath, err)
				}
			}
		} else {
			// If the disposition is 'inline' and has no filename, it's expected, log as debug.
			if strings.HasPrefix(contentDisposition, "inline") && params["filename"] == "" {
				log.Debugf("Content-Disposition is '%s' (no filename), using constructed filename.", contentDisposition)
			} else {
				// Log other parsing issues as warnings.
				log.WithError(err).Warnf("Could not parse Content-Disposition header: %s", contentDisposition)
			}
			// Keep finalFilepath as targetFilepath
		}
	} else {
		log.Warn("Warning: No Content-Disposition header found, using constructed filename.")
		// Keep finalFilepath as targetFilepath
	}
	// --- End Filename Handling ---

	// --- Prepend Model Version ID to Filename ---
	if modelVersionID > 0 { // Only prepend if ID is valid
		originalBase := filepath.Base(finalFilepath)
		dir := filepath.Dir(finalFilepath)
		newBase := fmt.Sprintf("%d_%s", modelVersionID, originalBase)
		finalFilepath = filepath.Join(dir, newBase)
		log.Debugf("Prepended model version ID, new final path: %s", finalFilepath)

		// Re-check existence for the *new* final path (with ID prepended)
		// This handles cases where the Content-Disposition was used, but a file
		// with the prepended ID *already* exists.
		if _, err := os.Stat(finalFilepath); err == nil {
			if helpers.CheckHash(finalFilepath, hashes) {
				log.Infof("File %s (with prepended ID) already exists and matches hash, skipping download.", finalFilepath)
				shouldCleanupTemp = true  // Still remove the new temp file we created
				return finalFilepath, nil // Return success
			} else {
				log.Warnf("File %s (with prepended ID) exists but hash mismatch, will overwrite.", finalFilepath)
				// No need to remove here, Rename will overwrite
			}
		} else if !os.IsNotExist(err) {
			// Error stating the potentially renamed file
			log.WithError(err).Errorf("Error checking status of final target file %s (with ID)", finalFilepath)
			return "", fmt.Errorf("%w: checking final target file %s: %v", ErrFileSystem, finalFilepath, err)
		}
	}
	// --- End Prepending ID ---

	// Get the size of the file
	size, _ := strconv.ParseUint(resp.Header.Get("Content-Length"), 10, 64)

	// Create a CounterWriter
	counter := &helpers.CounterWriter{
		Writer: tempFile,
		Total:  0,
	}

	// Write the body to temporary file, showing progress
	log.Infof("Downloading to %s (Target: %s, Size: %s)...", tempFile.Name(), finalFilepath, helpers.BytesToSize(size))
	_, err = io.Copy(counter, resp.Body)
	if err != nil {
		log.WithError(err).Errorf("Error writing temporary file %s", tempFile.Name())
		return "", fmt.Errorf("%w: writing temporary file %s: %v", ErrFileSystem, tempFile.Name(), err)
	}
	log.Infof("Finished writing %s.", tempFile.Name())

	// --- Hash Verification ---
	// Only verify if expected hashes were provided
	hashesProvided := hashes.SHA256 != "" || hashes.BLAKE3 != "" || hashes.CRC32 != "" || hashes.AutoV2 != ""
	if hashesProvided {
		log.Debugf("Verifying hash for %s...", tempFile.Name())
		if !helpers.CheckHash(tempFile.Name(), hashes) {
			log.Errorf("Verification failed for downloaded file %s. Hash mismatch.", tempFile.Name())
			// Temp file will be cleaned up by defer
			return "", ErrHashMismatch // Return specific error
		}
		log.Infof("Hash verified for %s.", tempFile.Name())
	} else {
		log.Debugf("Skipping hash verification for %s (no expected hashes provided).", tempFile.Name())
	}
	// --- End Hash Verification ---

	// Rename temporary file to final destination (finalFilepath)
	err = os.Rename(tempFile.Name(), finalFilepath)
	if err != nil {
		log.WithError(err).Errorf("Error renaming temporary file %s to %s", tempFile.Name(), finalFilepath)
		// Don't set shouldCleanupTemp = false, let defer handle removal of temp file
		return "", fmt.Errorf("%w: renaming temporary file %s to %s: %v", ErrFileSystem, tempFile.Name(), finalFilepath, err)
	}

	// If rename succeeded, prevent the deferred cleanup func from deleting the final file!
	shouldCleanupTemp = false

	log.Infof("Successfully downloaded and verified: %s", finalFilepath)
	return finalFilepath, nil // Success, no error
}
