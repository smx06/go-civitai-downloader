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

// Helper function to check for existing file by base name and hash
func findExistingFileWithMatchingBaseAndHash(dirPath string, baseNameWithoutExt string, hashes models.Hashes) (foundPath string, exists bool, err error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil // Directory doesn't exist, so file doesn't exist
		}
		return "", false, fmt.Errorf("reading directory %s: %w", dirPath, err)
	}

	log.Debugf("Scanning directory %s for base name '%s' with matching hash...", dirPath, baseNameWithoutExt)
	for _, entry := range entries {
		if entry.IsDir() {
			continue // Skip directories
		}
		entryName := entry.Name()
		ext := filepath.Ext(entryName)
		entryBaseName := strings.TrimSuffix(entryName, ext)

		if strings.EqualFold(entryBaseName, baseNameWithoutExt) {
			// Base names match
			fullPath := filepath.Join(dirPath, entryName)

			// Check if any standard hashes are provided
			hashesProvided := hashes.SHA256 != "" || hashes.BLAKE3 != "" || hashes.CRC32 != "" || hashes.AutoV2 != ""

			if !hashesProvided {
				// No standard hashes provided (likely an image), base name match is enough
				log.Debugf("Base name match found and no standard hashes provided. Assuming valid existing file: %s", fullPath)
				return fullPath, true, nil
			} else {
				// Hashes ARE provided, perform the check
				log.Debugf("Base name match found: %s. Checking hash...", fullPath)
				if helpers.CheckHash(fullPath, hashes) {
					log.Debugf("Hash match successful for existing file: %s", fullPath)
					return fullPath, true, nil // Found a valid match!
				} else {
					log.Debugf("Hash mismatch for existing file with matching base name: %s", fullPath)
					// Continue checking other files in case of duplicates with different content
				}
			}
		}
	}

	log.Debugf("No valid existing file found matching base name '%s' in %s", baseNameWithoutExt, dirPath)
	return "", false, nil // No matching file found
}

// DownloadFile downloads a file from the specified URL to the target filepath.
// It checks for existing files, verifies hashes, and attempts to use the
// Content-Disposition header for the filename.
// It also now accepts a modelVersionID to prepend to the final filename.
// Returns the final filepath used (or empty string on failure) and an error if one occurred.
func (d *Downloader) DownloadFile(targetFilepath string, url string, hashes models.Hashes, modelVersionID int) (string, error) {
	initialFinalFilepath := targetFilepath // Store the initially constructed path
	targetDir := filepath.Dir(initialFinalFilepath)
	initialBaseName := filepath.Base(initialFinalFilepath)
	initialExt := filepath.Ext(initialBaseName)
	initialBaseNameWithoutExt := strings.TrimSuffix(initialBaseName, initialExt)

	log.Debugf("Checking for existing file based on initial path: Dir=%s, BaseName=%s", targetDir, initialBaseNameWithoutExt)
	// --- Initial Check for Existing File (using new helper) ---
	foundPath, exists, errCheck := findExistingFileWithMatchingBaseAndHash(targetDir, initialBaseNameWithoutExt, hashes)
	if errCheck != nil {
		log.WithError(errCheck).Errorf("Error during initial check for existing file matching %s in %s", initialBaseNameWithoutExt, targetDir)
		return "", fmt.Errorf("%w: initial check for existing file: %v", ErrFileSystem, errCheck)
	}
	if exists {
		log.Infof("Found valid existing file matching base name '%s': %s. Skipping download.", initialBaseNameWithoutExt, foundPath)
		return foundPath, nil // Success, return the path of the valid existing file
	}
	log.Infof("No valid file matching base name '%s' found initially. Proceeding with download process.", initialBaseNameWithoutExt)
	// --- End Initial Check ---

	// Ensure target directory exists before creating temp file
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

	log.Info("Starting download process...") // Log before creating temp file

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
	potentialApiFilename := "" // Store potential filename from header
	if contentDisposition != "" {
		_, params, err := mime.ParseMediaType(contentDisposition)
		if err == nil && params["filename"] != "" {
			potentialApiFilename = params["filename"]
			log.Infof("Received filename from Content-Disposition: %s", potentialApiFilename)
		} else {
			// If the disposition is 'inline' and has no filename, it's expected, log as debug.
			if strings.HasPrefix(contentDisposition, "inline") && params["filename"] == "" {
				log.Debugf("Content-Disposition is '%s' (no filename), using constructed filename.", contentDisposition)
			} else {
				// Log other parsing issues as warnings.
				log.WithError(err).Warnf("Could not parse Content-Disposition header: %s", contentDisposition)
			}
		}
	} else {
		log.Warn("Warning: No Content-Disposition header found, will use constructed filename.")
	}

	// Determine the base filename to use (API provided or original)
	var baseFilenameToUse string
	if potentialApiFilename != "" {
		baseFilenameToUse = potentialApiFilename
	} else {
		baseFilenameToUse = filepath.Base(targetFilepath) // Use original base filename
	}
	// Construct the path *before* prepending ID
	pathBeforeId := filepath.Join(filepath.Dir(targetFilepath), baseFilenameToUse)

	var finalFilepath string // Declare finalFilepath here
	// --- Prepend Model Version ID to Filename ---
	if modelVersionID > 0 { // Only prepend if ID is valid
		finalFilepath = filepath.Join(filepath.Dir(pathBeforeId), fmt.Sprintf("%d_%s", modelVersionID, baseFilenameToUse))
		log.Debugf("Prepended model version ID, final target path: %s", finalFilepath)
	} else {
		finalFilepath = pathBeforeId // Use the path without ID if ID is 0
		log.Debugf("Model version ID is 0, final target path: %s", finalFilepath)
	}

	// --- Check Existence of FINAL Path (with potential API name and ID, using new helper) ---
	finalTargetDir := filepath.Dir(finalFilepath)
	finalBaseName := filepath.Base(finalFilepath)
	finalExt := filepath.Ext(finalBaseName)
	finalBaseNameWithoutExt := strings.TrimSuffix(finalBaseName, finalExt)

	log.Debugf("Checking for existing file based on determined final path: Dir=%s, BaseName=%s", finalTargetDir, finalBaseNameWithoutExt)
	foundPathFinal, existsFinal, errCheckFinal := findExistingFileWithMatchingBaseAndHash(finalTargetDir, finalBaseNameWithoutExt, hashes)
	if errCheckFinal != nil {
		log.WithError(errCheckFinal).Errorf("Error during final check for existing file matching %s in %s", finalBaseNameWithoutExt, finalTargetDir)
		return "", fmt.Errorf("%w: final check for existing file: %v", ErrFileSystem, errCheckFinal)
	}
	if existsFinal {
		log.Infof("Found valid existing file matching final base name '%s': %s. Download not needed.", finalBaseNameWithoutExt, foundPathFinal)
		shouldCleanupTemp = true   // Ensure any temp file created before this check is removed
		return foundPathFinal, nil // Success, return the path of the valid existing file
	}
	log.Debugf("Final target file base name '%s' does not exist with valid hash. Proceeding with network download to temp file.", finalBaseNameWithoutExt)
	// --- End Final Path Check ---

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

	// --- MIME Type Detection and Filename Correction (for images primarily) --- START ---
	// Get the original extension
	originalExt := strings.ToLower(filepath.Ext(finalFilepath))
	// Map of known image MIME types to extensions
	mimeToExt := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/gif":  ".gif",
		"image/webp": ".webp",
		// Add more common *image* types if needed, avoid non-image types for auto-correction
	}

	// Read the first 512 bytes to detect content type
	buff := make([]byte, 512)
	// Need to reopen the file to read from the beginning after writing
	fRead, errRead := os.Open(tempFile.Name()) // Open the *temp* file for reading
	if errRead != nil {
		log.WithError(errRead).Warnf("Could not open temp file %s for MIME type detection, using original extension.", tempFile.Name())
	} else {
		_, errReadBytes := fRead.Read(buff)
		fRead.Close() // Close the read handle immediately
		if errReadBytes != nil && errReadBytes != io.EOF {
			log.WithError(errReadBytes).Warnf("Could not read from temp file %s for MIME type detection, using original extension.", tempFile.Name())
		} else {
			mimeType := http.DetectContentType(buff)
			// Extract the main type (e.g., "image/jpeg")
			mainMimeType := strings.Split(mimeType, ";")[0]

			if detectedExt, ok := mimeToExt[mainMimeType]; ok {
				log.Debugf("Detected MIME type: %s -> Extension: %s for %s", mimeType, detectedExt, tempFile.Name())
				if originalExt != detectedExt {
					newFinalPath := strings.TrimSuffix(finalFilepath, originalExt) + detectedExt
					log.Warnf("Original extension '%s' differs from detected '%s'. Correcting final path to: %s", originalExt, detectedExt, newFinalPath)
					finalFilepath = newFinalPath // Update the final path for the upcoming rename
				} else {
					log.Debugf("Original extension '%s' matches detected '%s'. No path correction needed.", originalExt, detectedExt)
				}
			} else {
				log.Debugf("Detected MIME type '%s' for %s is not in the recognized image map. Using original extension '%s'.", mimeType, tempFile.Name(), originalExt)
			}
		}
	}
	// --- MIME Type Detection and Filename Correction --- END ---

	// Rename temporary file to final path (which might have been corrected)
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
