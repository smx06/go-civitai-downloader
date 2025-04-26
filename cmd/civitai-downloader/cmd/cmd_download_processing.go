package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/models"

	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus"
)

// processPage filters downloads based on config and database status.
// It returns the list of downloads that should be queued and their total size.
func processPage(db *database.DB, pageDownloads []potentialDownload, cfg *models.Config) ([]potentialDownload, uint64) {
	downloadsToQueue := []potentialDownload{}
	var queuedSizeBytes uint64 = 0

	for _, pd := range pageDownloads {
		// Calculate DB Key using ModelVersion ID
		if pd.CleanedVersion.ID == 0 {
			log.Warnf("Skipping potential download %s for model %s - missing ModelVersion ID.", pd.File.Name, pd.ModelName)
			continue
		}
		// Use prefix "v_" to distinguish version keys
		dbKey := fmt.Sprintf("v_%d", pd.CleanedVersion.ID)

		// Check database
		// Get retrieves raw bytes, unmarshaling happens later if needed
		rawValue, err := db.Get([]byte(dbKey)) // Note: db.Get returns raw bytes

		shouldQueue := false
		// Use errors.Is to check for the specific ErrNotFound error from our database package
		if errors.Is(err, database.ErrNotFound) {
			log.Debugf("Model Version %d (Key: %s) not found in DB. Queuing for download.", pd.CleanedVersion.ID, dbKey)
			shouldQueue = true
			// Create initial entry using the correct DatabaseEntry fields
			newEntry := models.DatabaseEntry{
				ModelName:    pd.ModelName,
				ModelType:    pd.ModelType,
				Version:      pd.CleanedVersion,                // Store the cleaned version struct
				File:         pd.File,                          // Store the file struct
				Timestamp:    time.Now().Unix(),                // Use Unix timestamp for AddedAt
				Creator:      pd.Creator,                       // Store the creator struct
				Filename:     filepath.Base(pd.TargetFilepath), // Use the calculated filename
				Folder:       pd.Slug,                          // Use the calculated folder slug
				Status:       models.StatusPending,             // Use constant
				ErrorDetails: "",                               // Use correct field name
			}
			// Marshal the new entry to JSON before putting into DB
			entryBytes, marshalErr := json.Marshal(newEntry)
			if marshalErr != nil {
				log.WithError(marshalErr).Errorf("Failed to marshal new DB entry for key %s", dbKey)
				continue // Skip queuing if marshalling fails
			}
			// Put the marshalled bytes
			if errPut := db.Put([]byte(dbKey), entryBytes); errPut != nil {
				log.WithError(errPut).Errorf("Failed to add pending entry to DB for key %s", dbKey)
				// Decide if we should still attempt download? Maybe not.
				continue // Skip queuing if DB write fails
			}
		} else if err != nil {
			// Handle other potential DB errors during Get
			log.WithError(err).Errorf("Error checking database for key %s", dbKey)
			continue // Skip this file on DB error
		} else {
			// Entry exists, unmarshal it
			var entry models.DatabaseEntry
			if unmarshalErr := json.Unmarshal(rawValue, &entry); unmarshalErr != nil {
				log.WithError(unmarshalErr).Errorf("Failed to unmarshal existing DB entry for key %s", dbKey)
				continue // Skip if we can't parse the existing entry
			}

			log.Debugf("Model Version %d (Key: %s) found in DB with status: %s", entry.Version.ID, dbKey, entry.Status)
			switch entry.Status {
			case models.StatusDownloaded:
				log.Infof("Skipping %s (VersionID: %d, Key: %s) - Already marked as downloaded.", pd.TargetFilepath, pd.CleanedVersion.ID, dbKey)
				// Update fields that might change between runs
				entry.Folder = pd.Slug
				entry.Filename = filepath.Base(pd.TargetFilepath)
				entry.Version = pd.CleanedVersion // Update associated metadata version
				entry.File = pd.File              // Update file details (URL might change)
				// entry.Timestamp = time.Now().Unix() // Optionally update timestamp on check? No LastChecked field.

				entryBytes, marshalErr := json.Marshal(entry)
				if marshalErr != nil {
					log.WithError(marshalErr).Warnf("Failed to marshal updated downloaded entry %s", dbKey)
				} else if errUpdate := db.Put([]byte(dbKey), entryBytes); errUpdate != nil {
					log.WithError(errUpdate).Warnf("Failed to update metadata for downloaded entry %s", dbKey)
				}
				shouldQueue = false
			case models.StatusPending, models.StatusError:
				log.Infof("Re-queuing %s (VersionID: %d, Key: %s) - Status is %s.", pd.TargetFilepath, pd.CleanedVersion.ID, dbKey, entry.Status)
				shouldQueue = true
				// Update status back to Pending and clear error if any
				entry.Status = models.StatusPending
				entry.ErrorDetails = ""
				// Update fields that might change
				entry.Folder = pd.Slug
				entry.Filename = filepath.Base(pd.TargetFilepath)
				entry.Version = pd.CleanedVersion
				entry.File = pd.File
				// entry.Timestamp = time.Now().Unix() // Optionally update timestamp?

				entryBytes, marshalErr := json.Marshal(entry)
				if marshalErr != nil {
					log.WithError(marshalErr).Errorf("Failed to marshal entry for re-queue update %s", dbKey)
					shouldQueue = false // Don't queue if marshalling fails
				} else if errUpdate := db.Put([]byte(dbKey), entryBytes); errUpdate != nil {
					log.WithError(errUpdate).Errorf("Failed to update DB entry to Pending for key %s", dbKey)
					shouldQueue = false // Don't queue if update fails
				}
			default:
				log.Warnf("Skipping %s (VersionID: %d, Key: %s) - Unknown status '%s' in database.", pd.TargetFilepath, pd.CleanedVersion.ID, dbKey, entry.Status)
				shouldQueue = false
			}
		}

		if shouldQueue {
			downloadsToQueue = append(downloadsToQueue, pd)
			queuedSizeBytes += uint64(pd.File.SizeKB * 1024)
			log.Debugf("Added confirmed download to queue: %s (Model: %s)", pd.File.Name, pd.ModelName)
		}
	}

	return downloadsToQueue, queuedSizeBytes
}

// saveModelInfoFile saves the full model metadata to a .json file.
// It saves the file to basePath/model_info/{baseModelSlug}/{modelNameSlug}/{model.ID}.json.
func saveModelInfoFile(model models.Model, basePath string, baseModelSlug string, modelNameSlug string) error {
	// Construct the directory path including base model and model name slugs
	infoDirPath := filepath.Join(basePath, "model_info", baseModelSlug, modelNameSlug)

	// Ensure the directory exists
	if err := os.MkdirAll(infoDirPath, 0700); err != nil {
		log.WithError(err).Errorf("Failed to create model info directory: %s", infoDirPath)
		return fmt.Errorf("failed to create directory %s: %w", infoDirPath, err)
	}

	// Construct the file path
	filePath := filepath.Join(infoDirPath, fmt.Sprintf("%d.json", model.ID))

	// Marshal the full model info
	jsonData, jsonErr := json.MarshalIndent(model, "", "  ")
	if jsonErr != nil {
		log.WithError(jsonErr).Warnf("Failed to marshal full model info for model %d (%s)", model.ID, model.Name)
		return fmt.Errorf("failed to marshal model info for %d: %w", model.ID, jsonErr)
	}

	// Write the file (overwrite if exists)
	if writeErr := os.WriteFile(filePath, jsonData, 0600); writeErr != nil {
		log.WithError(writeErr).Warnf("Failed to write model info file %s", filePath)
		return fmt.Errorf("failed to write model info file %s: %w", filePath, writeErr)
	}

	log.Debugf("Saved full model info to %s", filePath)
	return nil
}

// downloadImages handles downloading a list of images to a specified directory.
func downloadImages(logPrefix string, images []models.ModelImage, baseDir string, imageDownloader *downloader.Downloader, writer *uilive.Writer) (successCount, failCount int) {
	if imageDownloader == nil {
		log.Warnf("[%s] Image downloader is nil, cannot download images.", logPrefix)
		return 0, len(images) // Count all as failed if downloader doesn't exist
	}
	if len(images) == 0 {
		log.Debugf("[%s] No images provided to download.", logPrefix)
		return 0, 0
	}

	log.Debugf("[%s] Attempting to download %d images to %s", logPrefix, len(images), baseDir)

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		log.WithError(err).Errorf("[%s] Failed to create base directory for images: %s", logPrefix, baseDir)
		return 0, len(images) // Cannot proceed, count all as failed
	}

	for imgIdx, image := range images {
		// Construct image filename: {imageID}.{ext}
		imgUrlParsed, urlErr := url.Parse(image.URL) // Need import "net/url"
		var imgFilename string
		if urlErr != nil || image.ID == 0 {
			log.WithError(urlErr).Warnf("[%s] Cannot determine filename/ID for image %d (URL: %s). Using index.", logPrefix, imgIdx, image.URL)
			imgFilename = fmt.Sprintf("image_%d.jpg", imgIdx) // Fallback
		} else {
			ext := filepath.Ext(imgUrlParsed.Path)
			if ext == "" || len(ext) > 5 { // Basic check for valid extension
				log.Warnf("[%s] Image URL %s has unusual/missing extension '%s', defaulting to .jpg", logPrefix, image.URL, ext)
				ext = ".jpg"
			}
			imgFilename = fmt.Sprintf("%d%s", image.ID, ext)
		}
		imgTargetPath := filepath.Join(baseDir, imgFilename)

		// Check if image exists already
		if _, statErr := os.Stat(imgTargetPath); statErr == nil {
			log.Debugf("[%s] Skipping image %s - already exists.", logPrefix, imgFilename)
			// Should we count existing as success? For now, let's not increment counts.
			continue
		}

		log.Debugf("[%s] Downloading image %s from %s to %s", logPrefix, imgFilename, image.URL, imgTargetPath)
		if writer != nil { // Optional progress update // Need import "github.com/gosuri/uilive"
			fmt.Fprintf(writer.Newline(), "[%s] Downloading image %s...\n", logPrefix, imgFilename)
		}
		// Download the image (no hash check needed, no version ID prefix needed)
		_, dlErr := imageDownloader.DownloadFile(imgTargetPath, image.URL, models.Hashes{}, 0) // Need import "go-civitai-download/internal/downloader"
		if dlErr != nil {
			log.WithError(dlErr).Errorf("[%s] Failed to download image %s from %s", logPrefix, imgFilename, image.URL)
			if writer != nil {
				fmt.Fprintf(writer.Newline(), "[%s] Error image %s: %v\n", logPrefix, imgFilename, dlErr)
			}
			failCount++
		} else {
			log.Debugf("[%s] Downloaded image %s successfully.", logPrefix, imgFilename)
			if writer != nil {
				fmt.Fprintf(writer.Newline(), "[%s] Success image %s\n", logPrefix, imgFilename)
			}
			successCount++
		}
	}
	log.Debugf("[%s] Image download complete. Success: %d, Failed: %d", logPrefix, successCount, failCount)
	return successCount, failCount
}
