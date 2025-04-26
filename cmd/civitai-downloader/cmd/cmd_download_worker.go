package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
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

// downloadWorker handles the actual download of a file and updates the database.
// It now also accepts an imageDownloader to handle version images.
func downloadWorker(id int, jobs <-chan downloadJob, db *database.DB, fileDownloader *downloader.Downloader, imageDownloader *downloader.Downloader, wg *sync.WaitGroup, writer *uilive.Writer) {
	defer wg.Done()
	log.Debugf("Worker %d starting", id)
	for job := range jobs {
		pd := job.PotentialDownload
		dbKey := job.DatabaseKey // Use the key passed in the job
		log.Infof("Worker %d: Starting download for %s", id, pd.TargetFilepath)
		fmt.Fprintf(writer.Newline(), "Worker %d: Preparing %s...\n", id, filepath.Base(pd.TargetFilepath))

		// Ensure directory exists
		dirPath := filepath.Dir(pd.TargetFilepath)
		if err := os.MkdirAll(dirPath, 0700); err != nil {
			log.WithError(err).Errorf("Worker %d: Failed to create directory %s", id, dirPath)
			// Update DB status to Error
			rawValue, errGet := db.Get([]byte(dbKey))
			if errGet != nil {
				log.WithError(errGet).Errorf("Worker %d: Failed to get DB entry %s for error update (mkdir)", id, dbKey)
			} else {
				var entry models.DatabaseEntry
				if unmarshalErr := json.Unmarshal(rawValue, &entry); unmarshalErr != nil {
					log.WithError(unmarshalErr).Errorf("Worker %d: Failed to unmarshal DB entry %s for error update (mkdir)", id, dbKey)
				} else {
					entry.Status = models.StatusError
					entry.ErrorDetails = fmt.Sprintf("Failed to create directory: %v", err)
					entryBytes, marshalErr := json.Marshal(entry)
					if marshalErr != nil {
						log.WithError(marshalErr).Errorf("Worker %d: Failed to marshal DB entry %s for error status (mkdir)", id, dbKey)
					} else if errPut := db.Put([]byte(dbKey), entryBytes); errPut != nil {
						log.WithError(errPut).Errorf("Worker %d: Failed to update DB entry %s to Error status (mkdir)", id, dbKey)
					}
				}
			}
			fmt.Fprintf(writer.Newline(), "Worker %d: Error creating directory for %s: %v\n", id, filepath.Base(pd.TargetFilepath), err)
			continue // Skip to next job
		}

		// --- Perform Download ---
		startTime := time.Now()
		fmt.Fprintf(writer.Newline(), "Worker %d: Downloading %s...\n", id, filepath.Base(pd.TargetFilepath))

		// Initiate download - it returns the final path and error
		// Pass the correct DownloadUrl field
		// Pass the ModelVersionID for filename modification
		finalPath, err := fileDownloader.DownloadFile(pd.TargetFilepath, pd.File.DownloadUrl, pd.File.Hashes, pd.ModelVersionID)

		// --- Update DB Based on Result ---
		rawValue, errGet := db.Get([]byte(dbKey)) // Get raw bytes first
		finalStatus := models.StatusError         // Assume error unless explicitly successful
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}

		if errGet != nil {
			log.WithError(errGet).Errorf("Worker %d: Failed to get DB entry %s for status update", id, dbKey)
			fmt.Fprintf(writer.Newline(), "Worker %d: DB Error getting entry %s after download attempt.\n", id, dbKey)
			// Even if we can't get the entry, log the download outcome
			if err != nil {
				log.WithError(err).Errorf("Worker %d: Download failed for %s (DB get failed)", id, pd.TargetFilepath)
				fmt.Fprintf(writer.Newline(), "Worker %d: Error downloading %s: %v (DB get failed)\n", id, filepath.Base(pd.TargetFilepath), err)
			} else {
				// Use finalPath in log message
				log.Infof("Worker %d: Download successful for %s (DB get failed)", id, finalPath)
				fmt.Fprintf(writer.Newline(), "Worker %d: Success downloading %s (DB get failed)\n", id, filepath.Base(finalPath))
			}
		} else {
			// Unmarshal the existing entry
			var entry models.DatabaseEntry
			if unmarshalErr := json.Unmarshal(rawValue, &entry); unmarshalErr != nil {
				log.WithError(unmarshalErr).Errorf("Worker %d: Failed to unmarshal DB entry %s for status update", id, dbKey)
				fmt.Fprintf(writer.Newline(), "Worker %d: DB Error unmarshalling entry %s after download attempt.\n", id, dbKey)
				// Log original download outcome
				if err != nil {
					fmt.Fprintf(writer.Newline(), "Worker %d: Original Download Error: %v\n", id, err)
				} else {
					fmt.Fprintf(writer.Newline(), "Worker %d: Original Download Success.\n", id)
				}
			} else {
				// Successfully unmarshalled, now update fields
				if err != nil {
					log.WithError(err).Errorf("Worker %d: Failed to download %s", id, pd.TargetFilepath)
					entry.Status = models.StatusError
					entry.ErrorDetails = errMsg // Use ErrorDetails
					finalStatus = models.StatusError
					fmt.Fprintf(writer.Newline(), "Worker %d: Error downloading %s: %v\n", id, filepath.Base(pd.TargetFilepath), err)

					// Attempt to remove partially downloaded file (downloader might already do this, but can be redundant)
					if removeErr := os.Remove(pd.TargetFilepath); removeErr != nil && !os.IsNotExist(removeErr) {
						log.WithError(removeErr).Warnf("Worker %d: Failed to remove potentially partial file %s after download error", id, pd.TargetFilepath)
					}
				} else {
					duration := time.Since(startTime)
					// Use finalPath reported by downloader
					log.Infof("Worker %d: Successfully downloaded %s in %v", id, finalPath, duration)
					entry.Status = models.StatusDownloaded
					entry.ErrorDetails = ""                   // Clear any previous error
					entry.Filename = filepath.Base(finalPath) // Update filename in DB if it changed due to Content-Disposition
					// Folder should generally not change, but update File struct just in case
					entry.File = pd.File
					entry.Version = pd.CleanedVersion

					finalStatus = models.StatusDownloaded
					// Verification is handled by the downloader now
					fmt.Fprintf(writer.Newline(), "Worker %d: Success downloading %s\n", id, filepath.Base(finalPath))
				}

				// Marshal updated entry back to JSON
				updatedEntryBytes, marshalErr := json.Marshal(entry)
				if marshalErr != nil {
					log.WithError(marshalErr).Errorf("Worker %d: Failed to marshal updated DB entry %s", id, dbKey)
					fmt.Fprintf(writer.Newline(), "Worker %d: DB Error marshalling status update for %s\n", id, entry.Filename)
				} else {
					// Save updated entry back to DB
					if errPut := db.Put([]byte(dbKey), updatedEntryBytes); errPut != nil {
						log.WithError(errPut).Errorf("Worker %d: Failed to update DB entry %s to status %s", id, dbKey, entry.Status)
						fmt.Fprintf(writer.Newline(), "Worker %d: DB Error saving status for %s\n", id, entry.Filename)
					}
				}
			}
		}

		// Optional: Add metadata file saving logic here if needed
		// Use globalConfig defined in download.go (or pass it in)
		if viper.GetBool("download.save_metadata") && finalStatus == models.StatusDownloaded { // Only save metadata on success
			if err := saveMetadataFile(pd, finalPath); err != nil {
				// Error already logged by saveMetadataFile
				fmt.Fprintf(writer.Newline(), "Worker %d: Error saving metadata for %s: %v\n", id, filepath.Base(finalPath), err)
			}
		} else if viper.GetBool("download.save_metadata") && finalStatus != models.StatusDownloaded {
			log.Debugf("Worker %d: Skipping metadata save for %s due to download failure.", id, pd.TargetFilepath)
		}

		// --- Download Version Images if Enabled and Successful ---
		saveVersionImages := viper.GetBool("download.save_version_images") // Viper returns only bool
		if saveVersionImages && finalStatus == models.StatusDownloaded {
			log.Infof("Worker %d: Downloading version images for %s (%s)...", id, pd.ModelName, pd.VersionName)
			modelFileDir := filepath.Dir(finalPath) // Use finalPath from model download
			versionImagesDir := filepath.Join(modelFileDir, "version_images", fmt.Sprintf("%d", pd.ModelVersionID))

			if len(pd.OriginalImages) > 0 {
				if err := os.MkdirAll(versionImagesDir, 0755); err != nil {
					log.WithError(err).Errorf("Worker %d: Failed to create directory for version images: %s", id, versionImagesDir)
				} else {
					for imgIdx, image := range pd.OriginalImages {
						// Construct image filename: {imageID}.{ext} (assuming jpg/png)
						imgUrlParsed, urlErr := url.Parse(image.URL)
						var imgFilename string
						if urlErr != nil || image.ID == 0 {
							log.WithError(urlErr).Warnf("Worker %d: Cannot determine filename/ID for image %d (URL: %s). Using index.", id, imgIdx, image.URL)
							imgFilename = fmt.Sprintf("image_%d.jpg", imgIdx) // Fallback
						} else {
							ext := filepath.Ext(imgUrlParsed.Path)
							if ext == "" {
								ext = ".jpg"
							}
							imgFilename = fmt.Sprintf("%d%s", image.ID, ext)
						}
						imgTargetPath := filepath.Join(versionImagesDir, imgFilename)

						// Check if image exists already
						if _, statErr := os.Stat(imgTargetPath); statErr == nil {
							log.Debugf("Worker %d: Skipping version image %s - already exists.", id, imgFilename)
							continue
						}

						fmt.Fprintf(writer.Newline(), "Worker %d: Downloading version image %s...\n", id, imgFilename)
						// Download the image (no hash check needed)
						_, dlErr := imageDownloader.DownloadFile(imgTargetPath, image.URL, models.Hashes{}, 0)
						if dlErr != nil {
							log.WithError(dlErr).Errorf("Worker %d: Failed to download version image %s from %s", id, imgFilename, image.URL)
							fmt.Fprintf(writer.Newline(), "Worker %d: Error version image %s: %v\n", id, imgFilename, dlErr)
						} else {
							log.Infof("Worker %d: Downloaded version image %s", id, imgFilename)
							fmt.Fprintf(writer.Newline(), "Worker %d: Success version image %s\n", id, imgFilename)
						}
					}
				}
			} else {
				log.Debugf("Worker %d: No version images found for %s (%s).", id, pd.ModelName, pd.VersionName)
			}
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

	// Marshal the cleaned version info
	jsonData, jsonErr := json.MarshalIndent(pd.CleanedVersion, "", "  ")
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
