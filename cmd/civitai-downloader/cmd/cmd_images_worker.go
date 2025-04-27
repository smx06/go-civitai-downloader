package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	index "go-civitai-download/index"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/helpers"
	"go-civitai-download/internal/models"

	"github.com/blevesearch/bleve/v2"
	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus"
)

// Represents an image download task
type imageJob struct {
	SourceURL string
	ImageID   int
	Metadata  models.ImageApiItem
}

// --- Helper to save metadata --- START ---
func saveMetadataJSON(id int, job imageJob, targetPath string, writer *uilive.Writer) {
	baseFilename := filepath.Base(targetPath)
	metadataPath := strings.TrimSuffix(targetPath, filepath.Ext(targetPath)) + ".json"
	jsonData, jsonErr := json.MarshalIndent(job.Metadata, "", "  ")
	if jsonErr != nil {
		log.WithError(jsonErr).Warnf("Worker %d: Failed to marshal image metadata for %s", id, baseFilename)
		fmt.Fprintf(writer.Newline(), "Worker %d: Error marshalling metadata for %s\n", id, baseFilename)
	} else {
		if writeErr := os.WriteFile(metadataPath, jsonData, 0644); writeErr != nil {
			log.WithError(writeErr).Warnf("Worker %d: Failed to write image metadata file %s", id, metadataPath)
			fmt.Fprintf(writer.Newline(), "Worker %d: Error writing metadata file for %s\n", id, baseFilename)
		} else {
			log.Infof("Worker %d: Saved image metadata to %s", id, metadataPath) // Info level for explicit save
			fmt.Fprintf(writer.Newline(), "Worker %d: Saved metadata for %s\n", id, baseFilename)
		}
	}
}

// --- Helper to save metadata --- END ---

// imageDownloadWorker handles the download of a single image.
// Added baseOutputDir and bleveIndex parameters.
func imageDownloadWorker(id int, jobs <-chan imageJob, downloader *downloader.Downloader, wg *sync.WaitGroup, writer *uilive.Writer, successCounter *int64, failureCounter *int64, saveMeta bool, baseOutputDir string, bleveIndex bleve.Index) {
	defer wg.Done()
	log.Debugf("Image Worker %d starting", id)
	for job := range jobs {

		// --- Construct Target Path --- START ---
		// Create subdirectory based on username
		authorSlug := helpers.ConvertToSlug(job.Metadata.Username)
		if authorSlug == "" {
			authorSlug = "unknown_author" // Fallback
		}
		// Add BaseModel subdirectory
		baseModelSlug := helpers.ConvertToSlug(job.Metadata.BaseModel)
		if baseModelSlug == "" {
			baseModelSlug = "unknown_base_model"
		}
		targetSubDir := filepath.Join(baseOutputDir, authorSlug, baseModelSlug) // Include baseModelSlug

		// Construct filename: {id}-{url_filename_base}.{ext}
		var filename string
		imgURLParsed, urlErr := url.Parse(job.SourceURL) // Need to import "net/url"
		if urlErr != nil {
			log.WithError(urlErr).Warnf("Worker %d: Could not parse image URL %s for image ID %d. Using generic filename.", id, job.SourceURL, job.ImageID)
			filename = fmt.Sprintf("%d.image", job.ImageID) // Fallback includes ID
		} else {
			base := filepath.Base(imgURLParsed.Path)
			ext := filepath.Ext(base)
			nameOnly := strings.TrimSuffix(base, ext)
			safeName := helpers.ConvertToSlug(nameOnly)
			if safeName == "" {
				safeName = "image"
			}
			if ext == "" {
				// Guess extension based on typical Civitai usage or headers if possible
				// For now, default to jpg
				ext = ".jpg"
				log.Debugf("Worker %d: Could not determine extension for %s (ID %d), defaulting to .jpg", id, base, job.ImageID)
			}
			filename = fmt.Sprintf("%d-%s%s", job.ImageID, safeName, ext)
		}

		// Ensure the target subdirectory exists
		if err := os.MkdirAll(targetSubDir, 0755); err != nil {
			log.WithError(err).Errorf("Worker %d: Failed to create target directory %s for image %d, skipping download.", id, targetSubDir, job.ImageID)
			fmt.Fprintf(writer.Newline(), "Worker %d: Error creating dir for %s, skipping\n", id, filename)
			atomic.AddInt64(failureCounter, 1) // Count as failure
			continue
		}

		targetPath := filepath.Join(targetSubDir, filename)
		// --- Construct Target Path --- END ---

		baseFilename := filepath.Base(targetPath) // Use calculated base filename
		fmt.Fprintf(writer.Newline(), "Worker %d: Preparing %s (ID: %d)...\n", id, baseFilename, job.ImageID)

		// Check if image file already exists
		if _, err := os.Stat(targetPath); err == nil {
			log.Infof("Worker %d: Image file %s (ID: %d) already exists.", id, baseFilename, job.ImageID)
			// If file exists, check if metadata needs saving
			if saveMeta {
				metadataPath := strings.TrimSuffix(targetPath, filepath.Ext(targetPath)) + ".json"
				if _, metaErr := os.Stat(metadataPath); os.IsNotExist(metaErr) {
					log.Infof("Worker %d: Image exists, but metadata %s is missing. Saving metadata.", id, filepath.Base(metadataPath))
					saveMetadataJSON(id, job, targetPath, writer) // Call helper to save
				} else if metaErr == nil {
					log.Debugf("Worker %d: Metadata file %s also exists.", id, filepath.Base(metadataPath))
				} else {
					// Log error if stating metadata file failed for other reasons
					log.WithError(metaErr).Warnf("Worker %d: Could not check status of metadata file %s", id, metadataPath)
				}
			}
			// Skip the download
			fmt.Fprintf(writer.Newline(), "Worker %d: Skipping %s (Exists)\n", id, baseFilename)
			continue // Skip download steps
		}

		// --- Download section (only runs if file doesn't exist) ---
		fmt.Fprintf(writer.Newline(), "Worker %d: Downloading %s (ID: %d)...\n", id, baseFilename, job.ImageID)
		startTime := time.Now()

		// Use DownloadFile with the constructed targetPath
		_, dlErr := downloader.DownloadFile(targetPath, job.SourceURL, models.Hashes{}, 0)

		if dlErr != nil {
			log.WithError(dlErr).Errorf("Worker %d: Failed to download image %s from %s", id, targetPath, job.SourceURL)
			fmt.Fprintf(writer.Newline(), "Worker %d: Error downloading %s: %v\n", id, baseFilename, dlErr)
			// Attempt to remove partial file
			if removeErr := os.Remove(targetPath); removeErr != nil && !os.IsNotExist(removeErr) {
				log.WithError(removeErr).Warnf("Worker %d: Failed to remove partial image %s after error", id, targetPath)
			}
			atomic.AddInt64(failureCounter, 1)
		} else {
			duration := time.Since(startTime)
			log.Infof("Worker %d: Successfully downloaded %s in %v", id, targetPath, duration)
			fmt.Fprintf(writer.Newline(), "Worker %d: Success downloading %s (%v)\n", id, baseFilename, duration.Round(time.Millisecond))
			// Increment success counter
			atomic.AddInt64(successCounter, 1)

			// --- Save Metadata if Enabled (after successful download) ---
			if saveMeta {
				saveMetadataJSON(id, job, targetPath, writer) // Call helper to save
			}
			// --- End Save Metadata ---

			// --- Index Item with Bleve --- START ---
			if bleveIndex != nil {
				// Extract data from meta with type assertions
				var tags []string
				var prompt string
				var modelName string // Field not directly available, might be in meta?

				if metaMap, ok := job.Metadata.Meta.(map[string]interface{}); ok && metaMap != nil {
					if p, ok := metaMap["prompt"].(string); ok {
						prompt = p
					}
					if t, ok := metaMap["tags"].([]interface{}); ok {
						for _, tagInterface := range t {
							if tagStr, ok := tagInterface.(string); ok {
								tags = append(tags, tagStr)
							}
						}
					}
					// Check for model name in meta (unlikely standard field)
					if mn, ok := metaMap["modelName"].(string); ok {
						modelName = mn
					} else if mn, ok := metaMap["model"].(string); ok { // Common alternative key
						modelName = mn
					}
				}

				itemToIndex := index.Item{
					ID:          fmt.Sprintf("img_%d", job.ImageID),
					Type:        "image",
					Name:        baseFilename, // Use the calculated filename
					Description: prompt,       // Use extracted prompt as description
					FilePath:    targetPath,
					ModelName:   modelName, // Use extracted model name if found
					BaseModel:   job.Metadata.BaseModel,
					CreatorName: job.Metadata.Username,
					Tags:        tags, // Use extracted tags
					Prompt:      prompt,
					NsfwLevel:   job.Metadata.NsfwLevel,
				}
				if indexErr := index.IndexItem(bleveIndex, itemToIndex); indexErr != nil {
					log.WithError(indexErr).Errorf("Worker %d: Failed to index downloaded image %s (ID: %s)", id, targetPath, itemToIndex.ID)
				} else {
					log.Debugf("Worker %d: Successfully indexed image %s (ID: %s)", id, targetPath, itemToIndex.ID)
				}
			}
			// --- Index Item with Bleve --- END ---
		}
	}
	log.Debugf("Image Worker %d finished", id)
	fmt.Fprintf(writer.Newline(), "Worker %d: Finished image job processing.\n", id)
}
