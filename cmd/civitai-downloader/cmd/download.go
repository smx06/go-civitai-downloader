package cmd

import (
	"bufio" // For user confirmation prompt
	"encoding/json"

	// Ensure errors package is imported
	"fmt" // Added for io.ReadAll
	"net" // Added for net.Dialer
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync" // Import sync package for WaitGroup
	"time"

	// Use aliased import if needed

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/models"

	// Ensure errors package is imported

	// Import bitcask for ErrKeyNotFound
	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus" // Use logrus
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	// For request hashing
	"go-civitai-download/internal/api"
)

// downloadCmd represents the download command
var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download models based on specified criteria",
	Long: `Downloads models from Civitai based on various filters like tags, usernames, model types, etc.
It checks for existing files based on a local database and saves metadata.`,
	Run: runDownload,
}

// Add persistent flags for logging level and format
var logLevel string
var logFormat string // e.g., "text", "json"
// var concurrencyLevel int // Variable to store concurrency level

// processPage moved to cmd_download_processing.go

// downloadWorker moved to cmd_download_worker.go

// saveMetadataFile moved to cmd_download_worker.go

// saveModelInfoFile moved to cmd_download_processing.go

// setupDownloadEnvironment handles the initialization of database, downloaders, and concurrency settings.
func setupDownloadEnvironment(cmd *cobra.Command, cfg *models.Config) (db *database.DB, fileDownloader *downloader.Downloader, imageDownloader *downloader.Downloader, concurrencyLevel int, err error) {
	// --- Database Setup ---
	dbPath := cfg.DatabasePath
	if dbPath == "" {
		if cfg.SavePath != "" {
			dbPath = filepath.Join(cfg.SavePath, "civitai_download_db")
			log.Warnf("DatabasePath not set in config, defaulting to: %s", dbPath)
		} else {
			err = fmt.Errorf("DatabasePath and SavePath are not set in config. Cannot determine database location")
			return
		}
	}
	log.Infof("Opening database at: %s", dbPath)
	db, err = database.Open(dbPath)
	if err != nil {
		err = fmt.Errorf("failed to open database: %w", err)
		return
	}
	log.Info("Database opened successfully.")

	// --- Concurrency & Downloader Setup ---
	concurrencyLevel, _ = cmd.Flags().GetInt("concurrency")
	if concurrencyLevel <= 0 {
		concurrencyLevel = cfg.Concurrency // Use renamed field
		if concurrencyLevel <= 0 {
			concurrencyLevel = 3 // Hardcoded fallback default
			log.Warnf("Concurrency not set or invalid in config/flags, using default: %d", concurrencyLevel)
		}
	}
	log.Infof("Using concurrency level: %d", concurrencyLevel)

	// --- Downloader Client Setup ---
	// Directly use the globalHttpTransport set up in root.go
	if globalHttpTransport == nil {
		// Fallback in case root command setup failed silently
		log.Error("Global HTTP transport not initialized, using default transport without logging.")
		globalHttpTransport = http.DefaultTransport // Use default as fallback
	}
	// Create client for file downloader using the global transport
	mainHttpClient := &http.Client{
		Timeout:   0, // Rely on transport timeouts
		Transport: globalHttpTransport,
	}
	fileDownloader = downloader.NewDownloader(mainHttpClient, cfg.ApiKey)

	// --- Setup Image Downloader ---
	if viper.GetBool("download.save_version_images") || viper.GetBool("download.save_model_images") {
		log.Debug("Image saving enabled, creating image downloader instance.")
		// Create a separate client instance for image downloader, but reuse the global transport
		imgHttpClient := &http.Client{
			Timeout:   0,
			Transport: globalHttpTransport,
		}
		imageDownloader = downloader.NewDownloader(imgHttpClient, cfg.ApiKey)
	}

	return
}

// handleMetadataOnlyMode handles the logic when --download-meta-only is specified.
// It saves metadata for the queued files and returns true if the program should exit.
func handleMetadataOnlyMode(downloadsToQueue []potentialDownload, cfg *models.Config) (shouldExit bool) {
	log.Info("--- Metadata-Only Mode Activated --- ")
	if len(downloadsToQueue) == 0 {
		log.Info("No new files found for which to save metadata.")
		return true // Exit cleanly
	}

	log.Infof("Attempting to save metadata for %d files...", len(downloadsToQueue))
	savedCount := 0
	failedCount := 0
	for _, pd := range downloadsToQueue {
		// --- Reconstruct the intended file path for metadata saving ---
		// This mirrors the logic that would happen during download to determine the final filename
		// before the .json suffix is added by saveMetadataFile.
		baseFilename := pd.FinalBaseFilename // e.g., my_model_v1.safetensors
		finalFilenameWithID := baseFilename
		if pd.ModelVersionID > 0 { // Prepend ID if available
			finalFilenameWithID = fmt.Sprintf("%d_%s", pd.ModelVersionID, baseFilename)
		}
		dir := filepath.Dir(pd.TargetFilepath) // Get the target directory
		// Construct the final path that the model file *would* have had
		finalPathForMeta := filepath.Join(dir, finalFilenameWithID)
		log.Debugf("Using base path for meta-only JSON derivation: %s", finalPathForMeta)
		// --- End Path Reconstruction ---

		// Pass the potential download struct and the reconstructed path
		err := saveMetadataFile(pd, finalPathForMeta)
		if err != nil {
			// Use ModelVersionID for logging
			log.Warnf("Failed to save metadata for %s (VersionID: %d): %v", pd.File.Name, pd.ModelVersionID, err)
			failedCount++
		} else {
			savedCount++
		}
		// Note: We don't change DB status here.
	}

	log.Infof("Metadata-only mode finished. Saved: %d, Failed: %d", savedCount, failedCount)
	return true // Exit after processing
}

// confirmDownload displays the download summary and prompts the user for confirmation.
// Returns true if the user confirms, false otherwise.
func confirmDownload(downloadsToQueue []potentialDownload) bool {
	if len(downloadsToQueue) == 0 {
		log.Info("No new files meet the criteria or need downloading.")
		return false // Nothing to confirm
	}

	// Calculate total size for confirmation
	var totalQueuedSizeBytes uint64 = 0
	for _, pd := range downloadsToQueue {
		// Cast SizeKB (float64) to uint64 before calculation
		totalQueuedSizeBytes += uint64(pd.File.SizeKB) * 1024 // Convert KB to Bytes
	}

	log.Infof("--- Download Summary ---")
	log.Infof("Total files to download: %d", len(downloadsToQueue))
	log.Infof("Total size: %.2f GB", float64(totalQueuedSizeBytes)/(1024*1024*1024))
	// List first few files for context
	maxFilesToShow := 5
	if len(downloadsToQueue) < maxFilesToShow {
		maxFilesToShow = len(downloadsToQueue)
	}
	log.Info("Files to be downloaded include:")
	for i := 0; i < maxFilesToShow; i++ {
		pd := downloadsToQueue[i]
		log.Infof("  - %s (%.2f MB)", pd.FinalBaseFilename, pd.File.SizeKB/1024.0)
	}
	if len(downloadsToQueue) > maxFilesToShow {
		log.Infof("  ... and %d more.", len(downloadsToQueue)-maxFilesToShow)
	}

	// Confirmation Prompt
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Proceed with download? (y/N): ")
	input, _ := reader.ReadString('\n')
	input = strings.ToLower(strings.TrimSpace(input))

	if input != "y" {
		log.Info("Download cancelled by user.")
		return false
	}
	return true
}

// executeDownloads manages the worker pool and queues download jobs.
func executeDownloads(downloadsToQueue []potentialDownload, db *database.DB, fileDownloader *downloader.Downloader, imageDownloader *downloader.Downloader, concurrencyLevel int, cfg *models.Config) {
	log.Info("--- Starting Phase 3: Download Execution --- ")

	// Initialize uilive writer for progress updates
	writer := uilive.New()
	writer.Start()
	defer writer.Stop() // Ensure writer stops even if there are errors

	var wg sync.WaitGroup
	downloadJobs := make(chan downloadJob, concurrencyLevel) // Buffered channel

	// Start download workers
	log.Infof("Starting %d download workers...", concurrencyLevel)
	for i := 0; i < concurrencyLevel; i++ {
		wg.Add(1)
		// Pass necessary components to the worker
		// Pass imageDownloader and writer
		go downloadWorker(i+1, downloadJobs, db, fileDownloader, imageDownloader, &wg, writer) // Pass imageDownloader
	}

	// Queue downloads
	queuedCount := 0
	failedToQueueCount := 0
	for _, pd := range downloadsToQueue {
		// --- Calculate DB Key and Check Preconditions ---
		// Ensure ModelVersion ID exists before calculating key and checking DB
		if pd.CleanedVersion.ID == 0 {
			log.Errorf("Cannot process download for %s (Model: %s) - CleanedVersion ID is missing! Skipping queue.", pd.File.Name, pd.ModelName)
			failedToQueueCount++
			continue
		}
		// Calculate key using version ID with prefix (as it was originally)
		dbKey := fmt.Sprintf("v_%d", pd.CleanedVersion.ID)

		// Check DB status before queueing (should be Pending)
		rawValue, errGet := db.Get([]byte(dbKey))
		if errGet != nil {
			log.Warnf("Failed to get DB entry %s before queueing download job for %s. Skipping queue.", dbKey, pd.FinalBaseFilename)
			failedToQueueCount++
			continue
		}
		var entry models.DatabaseEntry
		if errUnmarshal := json.Unmarshal(rawValue, &entry); errUnmarshal != nil {
			log.Warnf("Failed to unmarshal DB entry %s before queueing download job for %s. Skipping queue.", dbKey, pd.FinalBaseFilename)
			failedToQueueCount++
			continue
		}

		if entry.Status != models.StatusPending {
			log.Warnf("DB entry %s for %s is not in Pending state (Status: %s). Skipping queue.", dbKey, pd.FinalBaseFilename, entry.Status)
			failedToQueueCount++
			continue
		}

		// Add job to the channel
		job := downloadJob{
			PotentialDownload: pd,
			DatabaseKey:       dbKey,
		}
		downloadJobs <- job
		queuedCount++
	}

	close(downloadJobs) // Close channel once all jobs are sent
	log.Infof("Queued %d download jobs. Waiting for workers to finish... (%d jobs failed to queue)", queuedCount, failedToQueueCount)

	wg.Wait() // Wait for all workers to complete
	log.Info("--- Finished Phase 3: Download Execution --- ")
}

// runDownload is the main execution function for the download command.
func runDownload(cmd *cobra.Command, args []string) {
	initLogging() // Ensures logging is set up based on flags
	log.Info("Starting Civitai Downloader - Download Command")

	// Config is loaded by PersistentPreRunE in root.go
	// REMOVED: globalConfig = models.LoadConfig()

	// --- Initialize Environment ---
	db, fileDownloader, imageDownloader, concurrencyLevel, err := setupDownloadEnvironment(cmd, &globalConfig)
	if err != nil {
		log.Fatalf("Failed to set up download environment: %v", err)
	}
	defer func() {
		log.Info("Closing database.")
		if err := db.Close(); err != nil {
			log.Errorf("Error closing database: %v", err)
		}
	}()
	// --- End Environment Initialization ---

	// =============================================
	// Phase 1: Metadata Gathering & Filtering
	// =============================================
	// log.Info("--- Starting Phase 1: Metadata Gathering & DB Check ---") // REMOVED - Moved inside else block
	// Use a client with shorter timeouts for metadata API calls

	// --- Setup Metadata HTTP Client ---
	metadataTimeout := time.Duration(globalConfig.ApiClientTimeoutSec) * time.Second
	if metadataTimeout <= 0 {
		metadataTimeout = 30 * time.Second // Fallback default for metadata calls
	}
	// Create the custom transport tuned for API calls
	metadataTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second, // Shorter timeout for API responses
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   5, // Fewer idle connections needed
	}

	// Wrap the transport for logging if enabled (similar to root.go)
	var finalMetadataTransport http.RoundTripper = metadataTransport
	if globalConfig.LogApiRequests {
		log.Debug("API request logging enabled, wrapping metadata HTTP transport.")
		// Use the main api.log file for metadata calls as well
		logFilePath := "api.log"
		if globalConfig.SavePath != "" {
			if _, statErr := os.Stat(globalConfig.SavePath); statErr == nil {
				logFilePath = filepath.Join(globalConfig.SavePath, logFilePath)
			} else {
				log.Warnf("SavePath '%s' not found, saving %s to current directory.", globalConfig.SavePath, logFilePath)
			}
		}
		log.Infof("Metadata API logging will append to file: %s", logFilePath)
		// Need to import "go-civitai-download/internal/api"
		loggingMetaTransport, err := api.NewLoggingTransport(metadataTransport, logFilePath)
		if err != nil {
			log.WithError(err).Error("Failed to initialize API logging transport for metadata client, logging disabled for it.")
			// Keep finalMetadataTransport as metadataTransport
		} else {
			finalMetadataTransport = loggingMetaTransport // Use the wrapped transport
			// TODO: How to close this specific logging transport? The defer in root.go only handles globalHttpTransport.
			// Maybe NewLoggingTransport should return a closer, or we need a global registry?
			// For now, it might leak a file handle if logging is on for metadata.
		}
	}

	// Create the metadata client using the (potentially wrapped) transport
	metadataClient := &http.Client{
		Timeout:   metadataTimeout,        // Set client-level timeout
		Transport: finalMetadataTransport, // Use the final transport
	}
	// --- End Setup Metadata HTTP Client ---

	// Pass address of globalConfig
	queryParams := setupQueryParams(&globalConfig, cmd)

	// --- Apply config overrides from flags (for filtering, not API query) ---
	if cmd.Flags().Changed("pruned") {
		prunedFlag, _ := cmd.Flags().GetBool("pruned")
		globalConfig.Pruned = prunedFlag // Use renamed field
		log.Debugf("Overriding config Pruned with flag: %t", prunedFlag)
	}
	if cmd.Flags().Changed("fp16") {
		fp16Flag, _ := cmd.Flags().GetBool("fp16")
		globalConfig.Fp16 = fp16Flag // Use renamed field
		log.Debugf("Overriding config Fp16 with flag: %t", fp16Flag)
	}
	// We might need to do this for other flags that affect filtering but not the API query,
	// e.g., primary-only, save-metadata, etc., if they weren't already handled in root.go
	// Let's check primary-only specifically, as it affects filtering
	if cmd.Flags().Changed("primary-only") {
		primaryOnlyFlag, _ := cmd.Flags().GetBool("primary-only")
		globalConfig.PrimaryOnly = primaryOnlyFlag // Use renamed field
		log.Debugf("Overriding config PrimaryOnly with flag: %t", primaryOnlyFlag)
	}
	// Add others as needed...
	// --- End Config Overrides ---

	// --- NEW: Check for specific model version ID ---
	modelVersionID, _ := cmd.Flags().GetInt("model-version-id")

	var downloadsToQueue []potentialDownload // Holds downloads confirmed for queueing after DB check
	var loopErr error                        // Store loop errors

	if modelVersionID > 0 {
		log.Infof("--- Processing specific Model Version ID: %d ---", modelVersionID)
		// Use the metadataClient initialized above
		// Call the handler function and ignore the returned size byte
		downloadsToQueue, _, loopErr = handleSingleVersionDownload(modelVersionID, db, metadataClient, &globalConfig, cmd)
		// REMOVED: log.Warnf("Single model version download (ID: %d) is not yet fully implemented.", modelVersionID)
		// REMOVED: loopErr = fmt.Errorf("single version download not implemented")

		if loopErr != nil {
			log.Errorf("Failed to process single model version %d: %v", modelVersionID, loopErr)
			return // Exit if single version fetch/process failed
		}
		log.Info("--- Finished processing single model version ---")
	} else {
		// --- Existing Pagination Logic (Code moved to fetchModelsPaginated) ---
		log.Info("--- Starting Phase 1: Metadata Gathering & DB Check --- (Pagination)")
		// Call the new function and ignore the returned size byte
		// Pass the imageDownloader instance for use with --save-model-images
		downloadsToQueue, _, loopErr = fetchModelsPaginated(db, metadataClient, imageDownloader, queryParams, &globalConfig, cmd)
		// REMOVED log.Warn("Pagination logic (fetchModelsPaginated) not yet fully integrated after move.")
		// REMOVED Placeholder to prevent proceeding without pagination logic
		// loopErr = fmt.Errorf("fetchModelsPaginated not implemented")

		if loopErr != nil {
			log.Errorf("Metadata gathering phase finished with error: %v", loopErr)
			log.Error("Aborting due to error during metadata gathering.")
			return
		}
		// REMOVED log.Info("--- Finished Phase 1: Metadata Gathering & DB Check ---")
	}

	// =============================================
	// Phase 1.5: Handle Metadata-Only Mode
	// =============================================
	metaOnly, _ := cmd.Flags().GetBool("meta-only") // Use renamed flag
	if metaOnly {
		if handleMetadataOnlyMode(downloadsToQueue, &globalConfig) {
			return // Exit if the handler function indicates we should.
		}
	}

	// =============================================
	// Phase 2: Summary & Confirmation
	// =============================================
	// Confirmation logic moved to confirmDownload function
	if !confirmDownload(downloadsToQueue) {
		return // Exit if user cancels
	}

	// =============================================
	// Phase 3: Download Execution
	// =============================================
	// Call the function to execute downloads
	executeDownloads(downloadsToQueue, db, fileDownloader, imageDownloader, concurrencyLevel, &globalConfig)

	// =============================================
	// Phase 4: Final Summary
	// =============================================
	log.Info("Download process complete.")
}
