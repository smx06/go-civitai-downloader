package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/helpers"
	"go-civitai-download/internal/models"

	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Define allowed values for image sorting and periods
var allowedImageSortOrders = map[string]bool{
	"Most Reactions": true,
	"Most Comments":  true,
	"Newest":         true,
}

var allowedImagePeriods = map[string]bool{
	"AllTime": true,
	"Year":    true,
	"Month":   true,
	"Week":    true,
	"Day":     true,
}

var allowedImageNsfwLevels = map[string]bool{
	"None":   true,
	"Soft":   true,
	"Mature": true,
	"X":      true,
	// Allow boolean true/false as well for the 'nsfw' param
	"true":  true,
	"false": true,
}

// imagesCmd represents the images command
var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Download images based on specified criteria from the /api/v1/images endpoint",
	Long: `Downloads images from Civitai based on filters like postId, modelId, username, nsfw level, etc.
Uses API pagination (nextPage URL) to fetch results.`,
	Run: runImages,
}

func init() {
	rootCmd.AddCommand(imagesCmd)

	// --- Flags for Image Command ---
	imagesCmd.Flags().Int("limit", 100, "Max images per page (1-200).")
	imagesCmd.Flags().Int("post-id", 0, "Filter by Post ID.")
	imagesCmd.Flags().Int("model-id", 0, "Filter by Model ID.")
	imagesCmd.Flags().Int("model-version-id", 0, "Filter by Model Version ID.")
	imagesCmd.Flags().StringP("username", "u", "", "Filter by username.")
	// Use string for nsfw flag to handle both boolean and enum values easily
	imagesCmd.Flags().String("nsfw", "", "Filter by NSFW level (None, Soft, Mature, X) or boolean (true/false). Empty means all.")
	imagesCmd.Flags().StringP("sort", "s", "Newest", "Sort order (Most Reactions, Most Comments, Newest).")
	imagesCmd.Flags().StringP("period", "p", "AllTime", "Time period for sorting (AllTime, Year, Month, Week, Day).")
	imagesCmd.Flags().Int("page", 1, "Starting page number (API defaults to 1).") // API uses page-based for images
	imagesCmd.Flags().Int("max-pages", 0, "Maximum number of API pages to fetch (0 for no limit)")
	imagesCmd.Flags().StringP("output-dir", "o", "", "Directory to save images (default: [SavePath]/images).")
	// Define a local variable for image command's concurrency flag
	var imageConcurrency int
	imagesCmd.Flags().IntVarP(&imageConcurrency, "concurrency", "c", 4, "Number of concurrent image downloads")
	// Add the save-metadata flag
	imagesCmd.Flags().Bool("save-metadata", false, "Save a .json metadata file alongside each downloaded image (overrides config).")
	// imagesCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt (if any needed - likely not for images)") // Remove unused flag

	// Bind flags to Viper (optional)
	viper.BindPFlag("images.limit", imagesCmd.Flags().Lookup("limit"))
	viper.BindPFlag("images.postId", imagesCmd.Flags().Lookup("post-id"))
	viper.BindPFlag("images.modelId", imagesCmd.Flags().Lookup("model-id"))
	viper.BindPFlag("images.modelVersionId", imagesCmd.Flags().Lookup("model-version-id"))
	viper.BindPFlag("images.username", imagesCmd.Flags().Lookup("username"))
	viper.BindPFlag("images.nsfw", imagesCmd.Flags().Lookup("nsfw"))
	viper.BindPFlag("images.sort", imagesCmd.Flags().Lookup("sort"))
	viper.BindPFlag("images.period", imagesCmd.Flags().Lookup("period"))
	viper.BindPFlag("images.page", imagesCmd.Flags().Lookup("page"))
	viper.BindPFlag("images.max_pages", imagesCmd.Flags().Lookup("max-pages"))
	viper.BindPFlag("images.output_dir", imagesCmd.Flags().Lookup("output-dir"))
	viper.BindPFlag("images.concurrency", imagesCmd.Flags().Lookup("concurrency"))
	// Bind the new flag
	viper.BindPFlag("images.save_metadata", imagesCmd.Flags().Lookup("save-metadata"))
	// viper.BindPFlag("images.yes", imagesCmd.Flags().Lookup("yes")) // Remove binding for unused flag
}

// runImages is the main execution function for the images command.
func runImages(cmd *cobra.Command, args []string) {
	initLogging() // Ensures logging is set up based on flags
	log.Info("Starting Civitai Downloader - Images Command")

	// Config loaded by root command's PersistentPreRunE

	// Determine Output Directory
	outputDir, _ := cmd.Flags().GetString("output-dir")
	if outputDir == "" {
		if globalConfig.SavePath == "" {
			log.Fatal("Required configuration 'SavePath' is not set and --output-dir flag was not provided.")
		}
		outputDir = filepath.Join(globalConfig.SavePath, "images") // Default subdirectory
		log.Infof("Output directory not specified, using default: %s", outputDir)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(outputDir, 0755); err != nil { // Use 0755 for potential broader access needed for image viewers etc.
		log.Fatalf("Failed to create output directory %s: %v", outputDir, err)
	}
	log.Infof("Saving images to: %s", outputDir)

	// --- API Client Setup ---
	// Reuse the refactored createDownloaderClient
	apiClient := createDownloaderClient(10) // Use moderate concurrency for API calls

	// --- Image Downloader Setup ---
	// Read the concurrency value bound to the local variable
	imgConcurrency, _ := cmd.Flags().GetInt("concurrency") // This now reads the flag correctly
	if imgConcurrency <= 0 {
		imgConcurrency = 4 // Default concurrency for images
	}
	log.Infof("Using concurrency level for image downloads: %d", imgConcurrency)
	// Create a client specifically for image downloading using the refactored function
	imgDownloadClient := createDownloaderClient(imgConcurrency)
	// Image downloader needs the full downloader to handle potential redirects, etc.
	// Pass API key just in case some image URLs require it.
	imageFileDownloader := downloader.NewDownloader(imgDownloadClient, globalConfig.ApiKey)

	// --- Concurrency Setup ---
	jobs := make(chan imageJob, imgConcurrency*2) // Buffered channel
	var wg sync.WaitGroup
	writer := uilive.New()
	writer.Start()
	defer func() {
		log.Debug("Defer: Attempting to stop uilive writer...")
		writer.Stop() // Ensure writer stops
		log.Debug("Defer: Returned from writer.Stop().")
	}()

	// Declare counters before starting workers that need their addresses
	var imagesQueued int64 = 0
	var imagesSuccess int64 = 0
	var imagesFailed int64 = 0

	// Start workers
	log.Infof("Starting %d image download workers...", imgConcurrency)
	for w := 1; w <= imgConcurrency; w++ {
		wg.Add(1)
		// Pass pointers to the atomic counters
		go imageDownloadWorker(w, jobs, imageFileDownloader, &wg, writer, &imagesSuccess, &imagesFailed)
	}

	// --- Parameter Setup ---
	params := url.Values{} // Use url.Values for easier query building

	if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 && limit <= 200 {
		params.Set("limit", strconv.Itoa(limit))
	} else if limit != 100 { // Only log if invalid value was explicitly provided
		log.Warnf("Invalid limit %d, using API default (100)", limit)
	}

	if postID, _ := cmd.Flags().GetInt("post-id"); postID > 0 {
		params.Set("postId", strconv.Itoa(postID))
	}
	if modelID, _ := cmd.Flags().GetInt("model-id"); modelID > 0 {
		params.Set("modelId", strconv.Itoa(modelID))
	}
	if modelVersionID, _ := cmd.Flags().GetInt("model-version-id"); modelVersionID > 0 {
		params.Set("modelVersionId", strconv.Itoa(modelVersionID))
	}
	if username, _ := cmd.Flags().GetString("username"); username != "" {
		params.Set("username", username)
	}
	if nsfw, _ := cmd.Flags().GetString("nsfw"); nsfw != "" {
		nsfwLower := strings.ToLower(nsfw)
		if _, ok := allowedImageNsfwLevels[nsfwLower]; ok {
			// Map boolean strings to actual boolean values expected by API for 'nsfw' param
			if nsfwLower == "true" {
				params.Set("nsfw", "true")
			} else if nsfwLower == "false" {
				params.Set("nsfw", "false")
			} else {
				// For enum values, use the 'nsfw' param as well (API seems flexible)
				// Or should we use a separate 'nsfwLevel' param if API supports it? Docs are ambiguous.
				// Let's assume 'nsfw' accepts the enums for now based on typical API behavior.
				params.Set("nsfw", nsfw) // Pass enum value directly
			}
		} else {
			log.Warnf("Invalid nsfw value '%s', ignoring filter.", nsfw)
		}
	}
	if sort, _ := cmd.Flags().GetString("sort"); sort != "" {
		if _, ok := allowedImageSortOrders[sort]; ok {
			params.Set("sort", sort)
		} else {
			log.Warnf("Invalid sort value '%s', using API default (Newest).", sort)
		}
	}
	if period, _ := cmd.Flags().GetString("period"); period != "" {
		if _, ok := allowedImagePeriods[period]; ok {
			params.Set("period", period)
		} else {
			log.Warnf("Invalid period value '%s', using API default (AllTime).", period)
		}
	}
	if page, _ := cmd.Flags().GetInt("page"); page > 0 {
		params.Set("page", strconv.Itoa(page))
	}

	// --- API Pagination Loop ---
	baseURL := "https://civitai.com/api/v1/images"
	currentURL := baseURL + "?" + params.Encode() // Initial URL
	pageCount := 0
	maxPages, _ := cmd.Flags().GetInt("max-pages")
	var loopErr error

	log.Info("--- Starting Image Fetching ---")

	for currentURL != "" {
		pageCount++
		if maxPages > 0 && pageCount > maxPages {
			log.Infof("Reached max pages limit (%d). Stopping.", maxPages)
			break
		}

		log.Debugf("Requesting Image URL (Page %d inferred): %s", pageCount, currentURL)

		req, err := http.NewRequest("GET", currentURL, nil)
		if err != nil {
			loopErr = fmt.Errorf("failed to create request for %s: %w", currentURL, err)
			break
		}
		if globalConfig.ApiKey != "" {
			req.Header.Add("Authorization", "Bearer "+globalConfig.ApiKey)
		}

		resp, err := apiClient.Do(req)
		if err != nil {
			if urlErr, ok := err.(*url.Error); ok && urlErr.Timeout() {
				log.WithError(err).Warnf("Timeout fetching image metadata page %d. Retrying after delay...", pageCount)
				time.Sleep(5 * time.Second) // Simple retry delay
				// Ideally, retry the *same* URL, not just continue the loop
				continue // Retry the current URL
			}
			loopErr = fmt.Errorf("failed to fetch image metadata page %d: %w", pageCount, err)
			break
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.WithError(closeErr).Warn("Error closing image API response body")
		}

		if readErr != nil {
			log.WithError(readErr).Errorf("Error reading image API response body for page %d, status %s", pageCount, resp.Status)
			loopErr = fmt.Errorf("failed to read response body (Page %d): %w", pageCount, readErr)
			// Should we try the next page if available? Maybe safer to break.
			break
		}

		if resp.StatusCode != http.StatusOK {
			errMsg := fmt.Sprintf("Image API request failed (Page %d inferred) with status %s", pageCount, resp.Status)
			if len(bodyBytes) > 0 {
				maxLen := 200
				bodyStr := string(bodyBytes)
				if len(bodyStr) > maxLen {
					bodyStr = bodyStr[:maxLen] + "..."
				}
				errMsg += fmt.Sprintf(". Response: %s", bodyStr)
			}
			log.Error(errMsg)
			if resp.StatusCode == http.StatusTooManyRequests {
				log.Warn("Rate limited. Applying longer delay...")
				delay := time.Duration(globalConfig.ApiDelayMs)*time.Millisecond*5 + 5*time.Second // Longer delay
				time.Sleep(delay)
				continue // Retry same URL
			}
			loopErr = fmt.Errorf(errMsg)
			break // Stop on other errors
		}

		var response models.ImageApiResponse
		if err := json.Unmarshal(bodyBytes, &response); err != nil {
			loopErr = fmt.Errorf("failed to decode image API response (Page %d): %w", pageCount, err)
			log.WithError(err).Errorf("Response body sample: %s", string(bodyBytes[:min(len(bodyBytes), 200)]))
			break
		}

		if len(response.Items) == 0 {
			log.Info("Received empty items list from API. Assuming end of results.")
			break
		}

		log.Infof("Received %d images from API page %d. Queueing downloads...", len(response.Items), pageCount)
		for _, image := range response.Items {
			// --- Path and Filename Construction ---
			// Sanitize author and base model for directory names
			authorSlug := helpers.ConvertToSlug(image.Username)
			if authorSlug == "" {
				authorSlug = "unknown_author"
			} // Fallback

			baseModelSlug := helpers.ConvertToSlug(image.BaseModel)
			if baseModelSlug == "" {
				baseModelSlug = "unknown_base_model"
			} // Fallback

			// Construct filename: {id}-{url_filename_base}.{ext}
			imgUrlParsed, urlErr := url.Parse(image.URL)
			var filename string
			if urlErr != nil {
				log.WithError(urlErr).Warnf("Could not parse image URL %s for image ID %d. Using generic filename.", image.URL, image.ID)
				filename = fmt.Sprintf("%d.image", image.ID) // Fallback includes ID
			} else {
				base := filepath.Base(imgUrlParsed.Path)
				ext := filepath.Ext(base)
				nameOnly := strings.TrimSuffix(base, ext)
				safeName := helpers.ConvertToSlug(nameOnly)
				if safeName == "" {
					safeName = "image"
				}
				if ext == "" {
					ext = ".jpg"
				} // Guess extension
				filename = fmt.Sprintf("%d-%s%s", image.ID, safeName, ext)
			}

			// Construct the full path: outputDir/authorSlug/baseModelSlug/filename
			targetDirPath := filepath.Join(outputDir, authorSlug, baseModelSlug)
			// Ensure the target directory exists (worker could also do this, but doing here is fine too)
			if err := os.MkdirAll(targetDirPath, 0755); err != nil {
				log.WithError(err).Errorf("Failed to create target directory %s for image %d, skipping queue.", targetDirPath, image.ID)
				continue // Skip this image if its directory cannot be created
			}
			targetPath := filepath.Join(targetDirPath, filename)
			// --- End Path/Filename Construction ---

			job := imageJob{
				SourceURL:  image.URL,
				TargetPath: targetPath,
				ImageID:    image.ID,
				Metadata:   image,
			}
			jobs <- job
			imagesQueued++
		}

		// --- Pagination ---
		if response.Metadata.NextPage != "" {
			currentURL = response.Metadata.NextPage
			log.Debugf("Next page URL found: %s", currentURL)
		} else {
			log.Info("No next page URL found in metadata. Finished fetching.")
			currentURL = "" // Stop the loop
		}

		// Apply polite delay
		if globalConfig.ApiDelayMs > 0 && currentURL != "" {
			log.Debugf("Applying API delay: %d ms", globalConfig.ApiDelayMs)
			time.Sleep(time.Duration(globalConfig.ApiDelayMs) * time.Millisecond)
		}
	} // End API loop

	if loopErr != nil {
		log.Errorf("Image fetching stopped due to error: %v", loopErr)
	} else {
		log.Info("--- Finished Image Fetching ---")
	}

	// --- Wait for Downloads ---
	log.Info("All image jobs queued. Waiting for workers to finish...")
	close(jobs) // Close channel once all jobs are sent
	log.Debug("Waiting on WaitGroup...")
	wg.Wait() // Wait for all workers to complete
	log.Debug("WaitGroup finished.")

	log.Info("--- Image Download Process Complete ---")
	// Report final count of successful downloads
	log.Infof("Finished. Queued: %d, Successful: %d, Failed: %d", imagesQueued, imagesSuccess, imagesFailed)
}

// Helper to create a client specifically for image downloads
// DEPRECATED: Use createDownloaderClient(concurrency) instead.
/*
func createDownloaderClientForImages(concurrency int) *http.Client {
	// Reuse logic from createDownloaderClient but use the specific concurrency
	clientTimeout := time.Duration(globalConfig.ApiClientTimeoutSec) * time.Second * 2 // Longer timeout for images?
	if clientTimeout <= 0 {
		clientTimeout = 120 * time.Second // Default 2 minutes
	}

	return &http.Client{
		Timeout: 0, // Rely on transport timeouts
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 60 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: clientTimeout, // Longer timeout
			MaxIdleConnsPerHost:   concurrency,   // Use image concurrency
		},
	}
}
*/

// Represents an image download task
type imageJob struct {
	SourceURL  string
	TargetPath string
	ImageID    int
	Metadata   models.ImageApiItem
}

// imageDownloadWorker handles the download of a single image.
// Added pointers to success and failure counters.
func imageDownloadWorker(id int, jobs <-chan imageJob, downloader *downloader.Downloader, wg *sync.WaitGroup, writer *uilive.Writer, successCounter *int64, failureCounter *int64) {
	defer wg.Done()
	log.Debugf("Image Worker %d starting", id)
	for job := range jobs {
		baseFilename := filepath.Base(job.TargetPath)
		fmt.Fprintf(writer.Newline(), "Worker %d: Preparing %s (ID: %d)...\n", id, baseFilename, job.ImageID)

		// Check if file already exists (simple check)
		if _, err := os.Stat(job.TargetPath); err == nil {
			log.Infof("Worker %d: Skipping image %s (ID: %d) - File already exists.", id, baseFilename, job.ImageID)
			fmt.Fprintf(writer.Newline(), "Worker %d: Skipping %s (Exists)\n", id, baseFilename)
			continue
		}

		fmt.Fprintf(writer.Newline(), "Worker %d: Downloading %s (ID: %d)...\n", id, baseFilename, job.ImageID)
		startTime := time.Now()

		// Use DownloadFile, ignoring hash and final path result
		// Provide nil for hashes and 0 for modelVersionID as they aren't relevant here.
		_, err := downloader.DownloadFile(job.TargetPath, job.SourceURL, models.Hashes{}, 0)

		if err != nil {
			log.WithError(err).Errorf("Worker %d: Failed to download image %s from %s", id, job.TargetPath, job.SourceURL)
			fmt.Fprintf(writer.Newline(), "Worker %d: Error downloading %s: %v\n", id, baseFilename, err)
			// Attempt to remove partial file
			if removeErr := os.Remove(job.TargetPath); removeErr != nil && !os.IsNotExist(removeErr) {
				log.WithError(removeErr).Warnf("Worker %d: Failed to remove partial image %s after error", id, job.TargetPath)
			}
			// Increment failure counter (Consider using atomic operation if adding across goroutines directly)
			// Since we read at the end after Wait(), direct increment in worker is okay, but atomic is safer practice.
			// Let's add atomic ops import and use them.
			// Need to pass pointers to the counters to the worker, or use a channel for results.
			// Simpler approach for now: Use atomics defined in runImages scope (requires passing pointers or modifying signature)
			// Even simpler: Do the final count based on logs? No, better to track directly.
			// Let's stick to atomic operations on shared counters.
			// --> We need to add `import "sync/atomic"` and pass counter pointers. (Done)
			// --> Rethink: Since wg.Wait() ensures all goroutines finish before we read the counts,
			// --> we don't strictly need atomics *if* we pass pointers and increment via the worker.
			// --> Let's modify worker signature to accept pointers to success/fail counters. (Done)
			atomic.AddInt64(failureCounter, 1)
		} else {
			duration := time.Since(startTime)
			log.Infof("Worker %d: Successfully downloaded %s in %v", id, job.TargetPath, duration)
			fmt.Fprintf(writer.Newline(), "Worker %d: Success downloading %s (%v)\n", id, baseFilename, duration.Round(time.Millisecond))
			// Increment success counter
			atomic.AddInt64(successCounter, 1)

			// --- Save Metadata if Enabled ---
			if globalConfig.SaveMetadata {
				metadataPath := strings.TrimSuffix(job.TargetPath, filepath.Ext(job.TargetPath)) + ".json"
				jsonData, jsonErr := json.MarshalIndent(job.Metadata, "", "  ")
				if jsonErr != nil {
					log.WithError(jsonErr).Warnf("Worker %d: Failed to marshal image metadata for %s", id, baseFilename)
					fmt.Fprintf(writer.Newline(), "Worker %d: Error marshalling metadata for %s\n", id, baseFilename)
				} else {
					if writeErr := os.WriteFile(metadataPath, jsonData, 0644); writeErr != nil {
						log.WithError(writeErr).Warnf("Worker %d: Failed to write image metadata file %s", id, metadataPath)
						fmt.Fprintf(writer.Newline(), "Worker %d: Error writing metadata file for %s\n", id, baseFilename)
					} else {
						log.Debugf("Worker %d: Saved image metadata to %s", id, metadataPath)
					}
				}
			}
			// --- End Save Metadata ---
		}
	}
	log.Debugf("Image Worker %d finished", id)
	fmt.Fprintf(writer.Newline(), "Worker %d: Finished image job processing.\n", id)
}
