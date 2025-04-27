package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/models"

	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// updateDbEntry encapsulates the logic for getting, updating, and putting a database entry.
// It takes the database connection, the key, the new status (string), and an optional function
// to apply further modifications to the entry before saving.
func updateDbEntry(db *database.DB, key string, newStatus string, updateFunc func(*models.DatabaseEntry)) error {
	rawValue, errGet := db.Get([]byte(key))
	if errGet != nil {
		// If the key isn't found, we can't update it. Log and return error.
		// If it's another error, log that too.
		log.WithError(errGet).Errorf("Failed to get DB entry '%s' for update", key)
		return fmt.Errorf("failed to get DB entry '%s': %w", key, errGet)
	}

	var entry models.DatabaseEntry
	if errUnmarshal := json.Unmarshal(rawValue, &entry); errUnmarshal != nil {
		log.WithError(errUnmarshal).Errorf("Failed to unmarshal DB entry '%s' for update", key)
		return fmt.Errorf("failed to unmarshal DB entry '%s': %w", key, errUnmarshal)
	}

	// Update the status
	entry.Status = newStatus

	// Apply additional modifications if provided
	if updateFunc != nil {
		updateFunc(&entry)
	}

	// Marshal updated entry back to JSON
	updatedEntryBytes, marshalErr := json.Marshal(entry)
	if marshalErr != nil {
		log.WithError(marshalErr).Errorf("Failed to marshal updated DB entry '%s' (Status: %s)", key, newStatus)
		return fmt.Errorf("failed to marshal DB entry '%s': %w", key, marshalErr)
	}

	// Save updated entry back to DB
	if errPut := db.Put([]byte(key), updatedEntryBytes); errPut != nil {
		log.WithError(errPut).Errorf("Failed to update DB entry '%s' to status %s", key, newStatus)
		return fmt.Errorf("failed to put DB entry '%s': %w", key, errPut)
	}

	log.Debugf("Successfully updated DB entry '%s' to status %s", key, newStatus)
	return nil
}

// handleMetadataSaving checks the config and calls saveMetadataFile if needed.
func handleMetadataSaving(logPrefix string, pd potentialDownload, finalPath string, finalStatus string, writer *uilive.Writer) {
	if viper.GetBool("savemetadata") {
		if finalStatus == models.StatusDownloaded {
			log.Debugf("[%s] Saving metadata for successfully downloaded file: %s", logPrefix, finalPath)
			if metaErr := saveMetadataFile(pd, finalPath); metaErr != nil {
				// Error already logged by saveMetadataFile
				if writer != nil {
					fmt.Fprintf(writer.Newline(), "[%s] Error saving metadata for %s: %v\n", logPrefix, filepath.Base(finalPath), metaErr)
				}
			}
		} else {
			log.Debugf("[%s] Skipping metadata save for %s due to download status: %s.", logPrefix, pd.TargetFilepath, finalStatus)
		}
	} else {
		log.Debugf("[%s] Skipping metadata save (disabled by config) for %s.", logPrefix, finalPath)
	}
}

// downloadWorker handles the actual download of a file and updates the database.
// It now also accepts an imageDownloader to handle version images and concurrencyLevel.
func downloadWorker(id int, jobs <-chan downloadJob, db *database.DB, fileDownloader *downloader.Downloader, imageDownloader *downloader.Downloader, wg *sync.WaitGroup, writer *uilive.Writer, concurrencyLevel int) {
	defer wg.Done()
	log.Debugf("Worker %d starting", id)
	for job := range jobs {
		pd := job.PotentialDownload
		dbKey := job.DatabaseKey // Use the key passed in the job
		log.Infof("Worker %d: Processing job for %s", id, pd.TargetFilepath)
		fmt.Fprintf(writer.Newline(), "Worker %d: Preparing %s...\n", id, filepath.Base(pd.TargetFilepath))

		// Ensure directory exists
		dirPath := filepath.Dir(pd.TargetFilepath)
		if err := os.MkdirAll(dirPath, 0700); err != nil {
			log.WithError(err).Errorf("Worker %d: Failed to create directory %s", id, dirPath)
			// Update DB status to Error using the helper
			updateErr := updateDbEntry(db, dbKey, models.StatusError, func(entry *models.DatabaseEntry) {
				entry.ErrorDetails = fmt.Sprintf("Failed to create directory: %v", err)
			})
			if updateErr != nil {
				// Log the error from the helper function
				log.Errorf("Worker %d: Failed to update DB status after mkdir error: %v", id, updateErr)
			}
			fmt.Fprintf(writer.Newline(), "Worker %d: Error creating directory for %s: %v\n", id, filepath.Base(pd.TargetFilepath), err)
			continue // Skip to next job
		}

		// --- Perform Download ---
		startTime := time.Now()
		fmt.Fprintf(writer.Newline(), "Worker %d: Checking/Downloading %s...\n", id, filepath.Base(pd.TargetFilepath))

		// Initiate download - it returns the final path and error
		finalPath, downloadErr := fileDownloader.DownloadFile(pd.TargetFilepath, pd.File.DownloadUrl, pd.File.Hashes, pd.ModelVersionID)

		// --- Update DB Based on Result ---
		finalStatus := models.StatusError // Default to error
		errMsg := ""
		if downloadErr != nil {
			errMsg = downloadErr.Error()
			finalStatus = models.StatusError
		} else {
			finalStatus = models.StatusDownloaded
		}

		// Use the helper function to update the DB entry
		updateErr := updateDbEntry(db, dbKey, finalStatus, func(entry *models.DatabaseEntry) {
			if downloadErr != nil {
				// Update error details on failure
				entry.ErrorDetails = errMsg
				log.WithError(downloadErr).Errorf("Worker %d: Failed to download %s", id, pd.TargetFilepath)
				fmt.Fprintf(writer.Newline(), "Worker %d: Error downloading %s: %v\n", id, filepath.Base(pd.TargetFilepath), downloadErr)

				// Attempt to remove partially downloaded file
				if removeErr := os.Remove(pd.TargetFilepath); removeErr != nil && !os.IsNotExist(removeErr) {
					log.WithError(removeErr).Warnf("Worker %d: Failed to remove potentially partial file %s after download error", id, pd.TargetFilepath)
				}
			} else {
				// Update fields on success
				duration := time.Since(startTime)
				log.Infof("Worker %d: Successfully downloaded %s in %v", id, finalPath, duration)
				entry.ErrorDetails = ""                   // Clear any previous error
				entry.Filename = filepath.Base(finalPath) // Update filename in DB
				entry.File = pd.File                      // Update File struct
				entry.Version = pd.CleanedVersion         // Update Version struct
				fmt.Fprintf(writer.Newline(), "Worker %d: Success downloading %s\n", id, filepath.Base(finalPath))
			}
		})

		if updateErr != nil {
			// Log error from the helper function, but continue with other tasks like image download if download was successful
			log.Errorf("Worker %d: Failed to update DB status after download attempt: %v", id, updateErr)
			fmt.Fprintf(writer.Newline(), "Worker %d: DB Error updating status for %s\n", id, pd.FinalBaseFilename)
		}

		// --- Metadata Saving ---
		logPrefix := fmt.Sprintf("Worker %d", id)
		handleMetadataSaving(logPrefix, pd, finalPath, finalStatus, writer)

		// --- Download Version Images if Enabled and Successful ---
		saveVersionImages := viper.GetBool("saveversionimages")
		if saveVersionImages && finalStatus == models.StatusDownloaded {
			logPrefix := fmt.Sprintf("Worker %d Img", id)
			log.Infof("[%s] Downloading version images for %s (%s)...", logPrefix, pd.ModelName, pd.VersionName)
			modelFileDir := filepath.Dir(finalPath) // Use finalPath from model download
			versionImagesDir := filepath.Join(modelFileDir, "images")

			// Add log before calling downloadImages
			log.Debugf("[%s] Calling downloadImages for %d images...", logPrefix, len(pd.OriginalImages))
			// Call the helper function, passing concurrencyLevel, removing writer
			imgSuccess, imgFail := downloadImages(logPrefix, pd.OriginalImages, versionImagesDir, imageDownloader, concurrencyLevel)
			log.Infof("[%s] Finished downloading version images for %s (%s). Success: %d, Failed: %d",
				logPrefix, pd.ModelName, pd.VersionName, imgSuccess, imgFail)
		}
		// --- End Download Version Images ---
	}
	log.Debugf("Worker %d finished", id)
	fmt.Fprintf(writer.Newline(), "Worker %d: Finished job processing.\n", id) // Final update for the worker
}

// saveMetadataFile saves the cleaned model version metadata to a .json file.
// It derives the metadata filename from the provided modelFilePath.
func saveMetadataFile(pd potentialDownload, modelFilePath string) error {
	// Calculate metadata path based on the model file path
	metadataPath := strings.TrimSuffix(modelFilePath, filepath.Ext(modelFilePath)) + ".json"
	// Ensure the target directory exists
	dirPath := filepath.Dir(metadataPath)
	if err := os.MkdirAll(dirPath, 0700); err != nil {
		log.WithError(err).Errorf("Failed to create directory for metadata file: %s", dirPath)
		return fmt.Errorf("failed to create directory %s: %w", dirPath, err)
	}

	// Marshal the full version info
	jsonData, jsonErr := json.MarshalIndent(pd.FullVersion, "", "  ")
	if jsonErr != nil {
		log.WithError(jsonErr).Warnf("Failed to marshal metadata for %s", modelFilePath)
		return fmt.Errorf("failed to marshal metadata for %s: %w", pd.ModelName, jsonErr)
	}

	// Write the file
	if writeErr := os.WriteFile(metadataPath, jsonData, 0600); writeErr != nil {
		log.WithError(writeErr).Warnf("Failed to write metadata file %s", metadataPath)
		return fmt.Errorf("failed to write metadata file %s: %w", metadataPath, writeErr)
	}

	log.Debugf("Saved metadata to %s", metadataPath)
	return nil
}
