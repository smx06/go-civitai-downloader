package cmd

import (
	"bufio" // For user confirmation prompt
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
	"go-civitai-download/internal/helpers"

	// Ensure errors package is imported

	// Import bitcask for ErrKeyNotFound
	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus" // Use logrus
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	// For request hashing
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

// createDownloaderClient creates an HTTP client with appropriate timeouts, concurrency settings,
// and the globally configured HTTP transport (which may include logging).
func createDownloaderClient(concurrency int) *http.Client {
	// Configure HTTP client with timeouts suitable for large file downloads
	// Use ApiClientTimeoutSec from global config
	clientTimeout := time.Duration(globalConfig.ApiClientTimeoutSec) * time.Second
	if clientTimeout <= 0 {
		clientTimeout = 60 * time.Second // Fallback default if config is invalid
		log.Warnf("Invalid ApiClientTimeoutSec (%d), using default: %v", globalConfig.ApiClientTimeoutSec, clientTimeout)
	}

	// --- IMPORTANT: Use the globally configured transport ---
	// The globalHttpTransport is set up in root.go and includes logging if enabled.
	// We might need to customize the *base* transport settings here if the global
	// one (http.DefaultTransport) isn't sufficient, but for now, let's assume it is.
	// If specific transport settings (like timeouts) are needed *per client*
	// beyond what the global transport provides, we might need to clone/
	// modify the global transport carefully.
	// For now, we directly use the global one.

	// If we needed to customize settings *based* on the global one:
	/*
	   customTransport := http.DefaultTransport.(*http.Transport).Clone() // Clone to modify safely
	   customTransport.ResponseHeaderTimeout = clientTimeout
	   // ... other specific settings ...
	   var finalTransport http.RoundTripper = customTransport
	   if globalConfig.LogApiRequests { // Check again? Or assume globalHttpTransport is already wrapped?
	       // Assume globalHttpTransport is already wrapped if logging is on.
	       finalTransport = globalHttpTransport // This seems wrong, we lose customizations.
	   }
	   // This suggests createDownloaderClient shouldn't exist if transport is global.
	   // Let's simplify: createDownloaderClient just makes a client using the GLOBAL transport.
	*/

	if globalHttpTransport == nil {
		// Fallback in case root command setup failed silently
		log.Error("Global HTTP transport not initialized, using default transport without logging.")
		globalHttpTransport = http.DefaultTransport
	}

	// TODO: Reconcile per-client settings (like ResponseHeaderTimeout) with global transport.
	// For now, the client timeout might not be respected if the global transport has its own.
	// A better approach might be for createDownloaderClient to ONLY return the transport,
	// and the calling code creates the client? Or pass transport settings to root setup?

	return &http.Client{
		Timeout:   0,                   // Rely on transport timeouts configured globally or below
		Transport: globalHttpTransport, // Use the globally configured transport
	}
	// Note: Settings like MaxIdleConnsPerHost previously set here might need to be
	// configured on the global transport during setup in root.go if they are
	// truly meant to be global, or applied carefully by cloning/modifying the transport.
	// Setting MaxIdleConnsPerHost per-client on a shared transport doesn't make sense.
}

// runDownload is the main execution function for the download command.
func runDownload(cmd *cobra.Command, args []string) {
	initLogging() // Ensures logging is set up based on flags
	log.Info("Starting Civitai Downloader - Download Command")

	// Config is loaded by PersistentPreRunE in root.go
	// REMOVED: globalConfig = models.LoadConfig()

	// --- Database Setup ---
	// Use DatabasePath directly from globalConfig
	dbPath := globalConfig.DatabasePath
	if dbPath == "" {
		// Attempt to construct default path if DatabasePath is empty in config
		if globalConfig.SavePath != "" {
			dbPath = filepath.Join(globalConfig.SavePath, "civitai_download_db") // Default filename
			log.Warnf("DatabasePath not set in config, defaulting to: %s", dbPath)
		} else {
			log.Fatalf("DatabasePath and SavePath are not set in config. Cannot determine database location.")
		}
	}
	log.Infof("Opening database at: %s", dbPath)
	// Use database.Open instead of database.NewDB
	db, err := database.Open(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer func() {
		log.Info("Closing database.")
		if err := db.Close(); err != nil {
			log.Errorf("Error closing database: %v", err)
		}
	}()
	log.Info("Database opened successfully.")
	// --- End Database Setup ---

	// --- Concurrency & Downloader Setup ---
	concurrencyLevel, _ := cmd.Flags().GetInt("concurrency")
	if concurrencyLevel <= 0 {
		// Use DefaultConcurrency from config, with a fallback
		concurrencyLevel = globalConfig.DefaultConcurrency
		if concurrencyLevel <= 0 {
			concurrencyLevel = 3 // Hardcoded fallback default
			log.Warnf("Concurrency not set or invalid in config/flags, using default: %d", concurrencyLevel)
		}
	}
	log.Infof("Using concurrency level: %d", concurrencyLevel)

	httpClient := createDownloaderClient(concurrencyLevel) // Use the function to create client
	// Pass API key from globalConfig to NewDownloader
	fileDownloader := downloader.NewDownloader(httpClient, globalConfig.ApiKey)
	// --- End Concurrency & Downloader Setup ---

	// --- Setup Image Downloader (used by workers and save-model-info) ---
	var imageDownloader *downloader.Downloader
	if viper.GetBool("download.save_version_images") || viper.GetBool("download.save_model_images") {
		log.Debug("Image saving enabled, creating image downloader instance.")
		// Create image downloader instance using the same concurrency level for simplicity
		imageDownloader = downloader.NewDownloader(createDownloaderClient(concurrencyLevel), globalConfig.ApiKey)
	}
	// --- End Image Downloader Setup ---

	// =============================================
	// Phase 1: Metadata Gathering & Filtering
	// =============================================
	// log.Info("--- Starting Phase 1: Metadata Gathering & DB Check ---") // REMOVED - Moved inside else block
	// Use a client with shorter timeouts for metadata API calls
	// Use ApiClientTimeoutSec from config for metadata client as well
	metadataTimeout := time.Duration(globalConfig.ApiClientTimeoutSec) * time.Second
	if metadataTimeout <= 0 {
		metadataTimeout = 30 * time.Second // Fallback default
	}
	metadataClient := &http.Client{
		Timeout: metadataTimeout, // Timeout for API calls
		Transport: &http.Transport{ // Reuse some transport settings but maybe less aggressive keep-alive
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second, // Shorter timeout for API responses (keep this shorter?)
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConnsPerHost:   5, // Fewer idle connections needed for API calls
		},
	}
	// Pass address of globalConfig
	queryParams := setupQueryParams(&globalConfig, cmd)

	// --- NEW: Check for specific model version ID ---
	modelVersionID, _ := cmd.Flags().GetInt("model-version-id")

	var downloadsToQueue []potentialDownload // Holds downloads confirmed for queueing after DB check
	var totalQueuedSizeBytes uint64 = 0      // Track size of queued downloads only
	var loopErr error                        // Store loop errors

	if modelVersionID > 0 {
		log.Infof("--- Processing specific Model Version ID: %d ---", modelVersionID)
		// Use the metadataClient initialized above
		// Call the handler function and store results
		downloadsToQueue, totalQueuedSizeBytes, loopErr = handleSingleVersionDownload(modelVersionID, db, metadataClient, &globalConfig, cmd)
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
		// Call the new function
		downloadsToQueue, totalQueuedSizeBytes, loopErr = fetchModelsPaginated(db, metadataClient, queryParams, &globalConfig, cmd)
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
	metaOnly, _ := cmd.Flags().GetBool("download-meta-only")
	if metaOnly {
		log.Info("--- Metadata-Only Mode Activated --- ")
		if len(downloadsToQueue) == 0 {
			log.Info("No new files found for which to save metadata.")
			return // Exit cleanly
		}

		log.Infof("Attempting to save metadata for %d files...", len(downloadsToQueue))
		savedCount := 0
		failedCount := 0
		for _, pd := range downloadsToQueue {
			// We use pd.TargetFilepath as the basis for the metadata filename,
			// BUT we need to prepend the version ID just like the downloader does,
			// AND use the FinalBaseFilename WITHOUT the metadata suffix.

			// Start with the final base filename (e.g., my_model_v1.safetensors)
			baseFilename := pd.FinalBaseFilename
			finalFilenameWithID := baseFilename // Default if no ID

			if pd.ModelVersionID > 0 { // Prepend ID if available
				finalFilenameWithID = fmt.Sprintf("%d_%s", pd.ModelVersionID, baseFilename)
			}

			// Get the target directory from the original TargetFilepath
			dir := filepath.Dir(pd.TargetFilepath)
			// Construct the final path for the metadata function (used to derive .json path)
			finalPathForMeta := filepath.Join(dir, finalFilenameWithID)
			log.Debugf("Using base path for meta-only JSON: %s", finalPathForMeta)

			// ---> ADDED DEBUG: Log the path being passed to saveMetadataFile
			log.Debugf("Passing path to saveMetadataFile (meta-only): %s", finalPathForMeta)
			if err := saveMetadataFile(pd, finalPathForMeta); err != nil {
				// Error is logged within saveMetadataFile
				failedCount++
			} else {
				savedCount++
			}
			// Note: We are *not* changing the DB status from Pending.
			// This allows a subsequent run without --download-meta-only to download the actual files.
		}

		log.Infof("Metadata saving complete. Success: %d, Failed: %d", savedCount, failedCount)
		log.Info("Skipping download phase due to --download-meta-only flag.")
		return // Exit after saving metadata
	}

	// =============================================
	// Phase 2: Summary & Confirmation
	// =============================================
	log.Info("--- Starting Phase 2: Summary & Confirmation --- ")

	if len(downloadsToQueue) == 0 { // Check the final queued list
		log.Info("No new files matching criteria found to download after checking database.")
		return // Exit cleanly
	}

	totalSizeStr := helpers.BytesToSize(totalQueuedSizeBytes) // Use helper function
	log.Infof("Found %d file(s) marked for download.", len(downloadsToQueue))
	log.Infof("Estimated total download size: %s", totalSizeStr)

	// Confirmation Prompt (skip if --yes flag is provided)
	skipConfirmation, _ := cmd.Flags().GetBool("yes")
	if !skipConfirmation {
		fmt.Printf("Proceed with download? (y/N): ")
		reader := bufio.NewReader(os.Stdin)
		confirm, _ := reader.ReadString('\n')
		confirm = strings.TrimSpace(strings.ToLower(confirm))

		if confirm != "y" {
			log.Info("Download cancelled by user.")
			// Update status of queued items back? Or leave as Pending?
			// Leaving as Pending seems reasonable. They might run again later.
			// Optionally, update DB for cancelled items:
			/*
				for _, pd := range downloadsToQueue {
					dbKey := database.CalculateKey(pd.File.Hashes.CRC32)
					entry, errGet := db.Get(dbKey)
					if errGet == nil && entry.Status == models.StatusPending {
						// entry.Status = models.StatusCancelled // Or just leave as Pending
						entry.LastChecked = time.Now()
						db.Put(dbKey, entry) // Ignore error?
					}
				}
			*/
			return
		}
		log.Info("User confirmed download.")
	} else {
		log.Info("Skipping confirmation prompt due to --yes flag.")
	}
	log.Info("--- Finished Phase 2: Summary & Confirmation --- ")

	// =============================================
	// Phase 3: Download Execution
	// =============================================
	log.Info("--- Starting Phase 3: Download Execution --- ")

	// Use uilive writer for progress updates
	writer := uilive.New()
	writer.Start() // Start the writer

	jobs := make(chan downloadJob, len(downloadsToQueue))
	var wg sync.WaitGroup

	// Start workers
	log.Infof("Starting %d download workers...", concurrencyLevel)
	// Create a single image downloader for all workers to share
	// imageDownloader := downloader.NewDownloader(createDownloaderClient(concurrencyLevel), globalConfig.ApiKey)

	for w := 1; w <= concurrencyLevel; w++ {
		wg.Add(1)
		// Pass db, fileDownloader, imageDownloader, wg, and writer to the worker
		go downloadWorker(w, jobs, db, fileDownloader, imageDownloader, &wg, writer)
	}

	// Send jobs to workers
	log.Infof("Queueing %d jobs for workers...", len(downloadsToQueue))
	queuedCount := 0
	for _, pd := range downloadsToQueue {
		// Ensure ModelVersion ID exists before calculating key and queueing
		if pd.CleanedVersion.ID == 0 {
			log.Errorf("Cannot queue download for %s (Model: %s) - ModelVersion ID is missing!", pd.File.Name, pd.ModelName)
			continue // Skip this item
		}
		// Calculate key using version ID with prefix
		dbKey := fmt.Sprintf("v_%d", pd.CleanedVersion.ID)
		job := downloadJob{
			PotentialDownload: pd,
			DatabaseKey:       dbKey, // Pass the key
		}
		jobs <- job
		queuedCount++
	}
	close(jobs) // Close channel once all jobs are sent

	if queuedCount != len(downloadsToQueue) {
		log.Warnf("Attempted to queue %d jobs, but only %d were sent (due to missing hashes?).", len(downloadsToQueue), queuedCount)
	}

	log.Info("All download jobs queued. Waiting for workers to finish...")
	wg.Wait() // Wait for all workers to complete

	writer.Stop() // Stop the writer IMPORTANT

	log.Info("--- Finished Phase 3: Download Execution --- ")
	log.Info("Download process complete.")
}
