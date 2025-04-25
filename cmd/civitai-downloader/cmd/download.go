package cmd

import (
	"bufio" // For user confirmation prompt
	"encoding/json"
	"errors" // Ensure errors package is imported
	"fmt"
	"io"  // Added for io.ReadAll
	"net" // Added for net.Dialer
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync" // Import sync package for WaitGroup
	"time"

	// Use aliased import if needed

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/helpers"
	"go-civitai-download/internal/models"

	// Ensure errors package is imported

	// Import bitcask for ErrKeyNotFound
	"github.com/gosuri/uilive"
	log "github.com/sirupsen/logrus" // Use logrus
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	// For request hashing
)

// Allowed values for API parameters
var allowedSortOrders = map[string]bool{
	"Highest Rated":   true,
	"Most Downloaded": true,
	"Newest":          true,
}

var allowedPeriods = map[string]bool{
	"AllTime": true,
	"Year":    true,
	"Month":   true,
	"Week":    true,
	"Day":     true,
}

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
var logFormat string     // e.g., "text", "json"
var concurrencyLevel int // Variable to store concurrency level

// potentialDownload holds information about a file identified during the metadata scan phase.
type potentialDownload struct {
	ModelName         string
	ModelType         string
	VersionName       string
	BaseModel         string
	Creator           models.Creator
	File              models.File // Contains URL, Hashes, SizeKB etc.
	ModelVersionID    int         // Add Model Version ID
	TargetFilepath    string      // Full calculated path for download
	Slug              string      // Folder structure
	FinalBaseFilename string      // Base filename part without ID prefix or metadata suffix (e.g., wan_cowgirl_v1.3.safetensors)
	// Store cleaned version separately for potential later use in DB entry
	CleanedVersion models.ModelVersion
}

// Represents a download task to be processed by a worker.
type downloadJob struct {
	PotentialDownload potentialDownload // Embed potential download info
	DatabaseKey       string            // Key for DB updates
}

func init() {
	// Add downloadCmd to rootCmd
	rootCmd.AddCommand(downloadCmd)

	// Add persistent flags to rootCmd so they apply to all commands
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Logging level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "Logging format (text, json)")

	// Hook to configure logging before any command runs
	cobra.OnInitialize(initLogging)

	// Define flags specific to the download command
	downloadCmd.Flags().StringSliceP("type", "t", []string{}, "Filter by model type(s) (e.g., Checkpoint, LORA). Overrides config.")
	downloadCmd.Flags().StringSliceP("base-model", "b", []string{}, "Filter by base model(s) (e.g., \"SD 1.5\", SDXL). Overrides config.")
	// Use BoolP for boolean flags, allows --nsfw=false or just --nsfw for true
	// Need to handle the case where the flag isn't set vs. set to false.
	// We can use GetBool to check if it was explicitly set.
	downloadCmd.Flags().Bool("nsfw", false, "Include NSFW models (true/false). Overrides config if set.")
	downloadCmd.Flags().IntP("limit", "l", 100, "Max models per page (1-100).")
	downloadCmd.Flags().StringP("sort", "s", "Most Downloaded", "Sort order (Most Downloaded, Highest Rated, Newest).")
	downloadCmd.Flags().StringP("period", "p", "AllTime", "Time period for sorting (AllTime, Year, Month, Week, Day).")
	downloadCmd.Flags().Bool("primary-only", false, "Only download primary model files. Overrides config if set.")
	downloadCmd.Flags().StringP("query", "q", "", "Search query string.")
	downloadCmd.Flags().StringP("tag", "", "", "Filter by tag name.") // No short flag for tag?
	downloadCmd.Flags().StringP("username", "u", "", "Filter by username.")
	// Config Override Flags (affect filtering logic, not API query params directly)
	downloadCmd.Flags().Bool("pruned", false, "Only download pruned Checkpoint models. Overrides config if set.")
	downloadCmd.Flags().Bool("fp16", false, "Only download fp16 Checkpoint models. Overrides config if set.")

	// Add concurrency flag
	downloadCmd.Flags().IntVarP(&concurrencyLevel, "concurrency", "c", 4, "Number of concurrent downloads")

	// Add flag for saving metadata
	downloadCmd.Flags().Bool("save-metadata", false, "Save a .json metadata file alongside each download. Overrides config.")

	// TODO: Add flags for other QueryParameters like allow*
	// TODO: Add flags for other Config fields like Ignore*

	// Add new flags for download command
	downloadCmd.Flags().StringSlice("tags", []string{}, "Filter by tags (comma-separated)")
	downloadCmd.Flags().StringSlice("usernames", []string{}, "Filter by usernames (comma-separated)")
	downloadCmd.Flags().StringSliceP("model-types", "m", []string{}, "Filter by model types (e.g., Checkpoint, LORA, LoCon)")
	downloadCmd.Flags().Int("max-pages", 0, "Maximum number of API pages to fetch (0 for no limit)")
	downloadCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	downloadCmd.Flags().Bool("download-meta-only", false, "Only download and save .json metadata files, skip model download.")
	downloadCmd.Flags().Bool("save-model-info", false, "Save full model info JSON to '[SavePath]/model_info/[model.ID].json'.")

	// Bind flags to Viper (optional, if you want config file overrides)
	viper.BindPFlag("download.tags", downloadCmd.Flags().Lookup("tags"))
	viper.BindPFlag("download.usernames", downloadCmd.Flags().Lookup("usernames"))
	viper.BindPFlag("download.model_types", downloadCmd.Flags().Lookup("model-types"))
	viper.BindPFlag("download.query", downloadCmd.Flags().Lookup("query"))
	viper.BindPFlag("download.sort", downloadCmd.Flags().Lookup("sort"))
	viper.BindPFlag("download.period", downloadCmd.Flags().Lookup("period"))
	viper.BindPFlag("download.limit", downloadCmd.Flags().Lookup("limit"))
	viper.BindPFlag("download.max_pages", downloadCmd.Flags().Lookup("max-pages"))
	viper.BindPFlag("download.concurrency", downloadCmd.Flags().Lookup("concurrency"))
	viper.BindPFlag("download.save-metadata", downloadCmd.Flags().Lookup("save-metadata"))
	viper.BindPFlag("download.primary-only", downloadCmd.Flags().Lookup("primary-only"))
	viper.BindPFlag("download.pruned", downloadCmd.Flags().Lookup("pruned"))
	viper.BindPFlag("download.fp16", downloadCmd.Flags().Lookup("fp16"))
	viper.BindPFlag("download.yes", downloadCmd.Flags().Lookup("yes"))
	viper.BindPFlag("download.download_meta_only", downloadCmd.Flags().Lookup("download-meta-only"))
	viper.BindPFlag("download.save_model_info", downloadCmd.Flags().Lookup("save-model-info"))
}

// initLogging configures logrus based on persistent flags
func initLogging() {
	level, err := log.ParseLevel(logLevel)
	if err != nil {
		log.WithError(err).Warnf("Invalid log level '%s', using default 'info'", logLevel)
		level = log.InfoLevel
	}
	log.SetLevel(level)

	switch logFormat {
	case "json":
		log.SetFormatter(&log.JSONFormatter{})
	case "text":
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	default:
		log.Warnf("Invalid log format '%s', using default 'text'", logFormat)
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	}

	log.Infof("Logging configured: Level=%s, Format=%s", log.GetLevel(), logFormat)
}

// setupQueryParams initializes the query parameters based on global config and flags.
func setupQueryParams(cfg *models.Config, cmd *cobra.Command) models.QueryParameters {
	params := models.QueryParameters{
		Limit:                  cfg.Limit, // Use cfg.Limit as default
		Page:                   1,         // Start at page 1
		Query:                  "",        // Initialize Query, Tag, Username
		Tag:                    "",
		Username:               "",
		Types:                  cfg.Types,               // Use Types from config
		Sort:                   cfg.Sort,                // Use Sort from config
		Period:                 cfg.Period,              // Use Period from config
		Rating:                 0,                       // Optional: Filter by rating
		Favorites:              false,                   // Optional: Filter by favorites
		Hidden:                 false,                   // Optional: Filter by hidden status
		PrimaryFileOnly:        cfg.GetOnlyPrimaryModel, // Use GetOnlyPrimaryModel from config
		AllowNoCredit:          true,                    // Default based on typical usage
		AllowDerivatives:       true,                    // Default based on typical usage
		AllowDifferentLicenses: true,                    // Default based on typical usage
		AllowCommercialUse:     "Any",                   // Default based on typical usage
		Nsfw:                   cfg.GetNsfw,             // Use GetNsfw from config
		BaseModels:             cfg.BaseModels,          // Use BaseModels from config
	}

	// Validate initial Sort from config
	if _, ok := allowedSortOrders[params.Sort]; !ok && params.Sort != "" { // Check if set and not allowed
		log.Warnf("Invalid Sort value '%s' in config, using default 'Most Downloaded'", params.Sort)
		params.Sort = "Most Downloaded"
	} else if params.Sort == "" { // Assign default if empty
		params.Sort = "Most Downloaded"
	}

	// Validate initial Period from config
	if _, ok := allowedPeriods[params.Period]; !ok && params.Period != "" { // Check if set and not allowed
		log.Warnf("Invalid Period value '%s' in config, using default 'AllTime'", params.Period)
		params.Period = "AllTime"
	} else if params.Period == "" { // Assign default if empty
		params.Period = "AllTime"
	}

	// Validate initial Limit from config
	if cfg.Limit <= 0 || cfg.Limit > 100 {
		if cfg.Limit != 0 { // Don't warn if it was just omitted (zero value)
			log.Warnf("Invalid Limit value '%d' in config, using default 100", cfg.Limit)
		}
		params.Limit = 100
	} else {
		params.Limit = cfg.Limit // Use valid config limit
	}

	// Override QueryParameter fields with flags if set
	if cmd.Flags().Changed("type") {
		types, _ := cmd.Flags().GetStringSlice("type")
		log.WithField("types", types).Debug("Overriding Types with flag value")
		params.Types = types
	}
	if cmd.Flags().Changed("base-model") {
		baseModels, _ := cmd.Flags().GetStringSlice("base-model")
		log.WithField("baseModels", baseModels).Debug("Overriding BaseModels with flag value")
		params.BaseModels = baseModels // Note: API uses 'baseModels', mapping handled in client
	}
	if cmd.Flags().Changed("nsfw") {
		nsfw, _ := cmd.Flags().GetBool("nsfw")
		log.WithField("nsfw", nsfw).Debug("Overriding Nsfw with flag value")
		params.Nsfw = nsfw
	}
	if cmd.Flags().Changed("limit") {
		limit, _ := cmd.Flags().GetInt("limit")
		if limit > 0 && limit <= 100 {
			log.Debugf("Overriding Limit with flag value: %d", limit)
			params.Limit = limit
		} else {
			log.Warnf("Invalid limit value '%d' from flag, ignoring flag and using value: %d", limit, params.Limit)
		}
	}
	if cmd.Flags().Changed("sort") {
		sort, _ := cmd.Flags().GetString("sort")
		if _, ok := allowedSortOrders[sort]; ok {
			log.Debugf("Overriding Sort with flag value: %s", sort)
			params.Sort = sort
		} else {
			log.Warnf("Invalid sort value '%s' from flag, ignoring flag and using value: %s", sort, params.Sort)
		}
	}
	if cmd.Flags().Changed("period") {
		period, _ := cmd.Flags().GetString("period")
		if _, ok := allowedPeriods[period]; ok {
			log.Debugf("Overriding Period with flag value: %s", period)
			params.Period = period
		} else {
			log.Warnf("Invalid period value '%s' from flag, ignoring flag and using value: %s", period, params.Period)
		}
	}
	if cmd.Flags().Changed("primary-only") {
		primaryOnly, _ := cmd.Flags().GetBool("primary-only")
		log.WithField("primaryOnly", primaryOnly).Debug("Overriding PrimaryFileOnly with flag value")
		params.PrimaryFileOnly = primaryOnly
	}

	if cmd.Flags().Changed("query") {
		query, _ := cmd.Flags().GetString("query")
		params.Query = query
		log.Debugf("Setting Query from flag: %s", query)
	}
	if cmd.Flags().Changed("tag") {
		tag, _ := cmd.Flags().GetString("tag")
		params.Tag = tag
		log.Debugf("Setting Tag from flag: %s", tag)
	}
	if cmd.Flags().Changed("username") {
		username, _ := cmd.Flags().GetString("username")
		params.Username = username
		log.Debugf("Setting Username from flag: %s", username)
	}

	// Handle Config Override Flags (these modify the *config* used for later filtering, not API params)
	if cmd.Flags().Changed("pruned") {
		prunedFlag, _ := cmd.Flags().GetBool("pruned")
		cfg.GetPruned = prunedFlag // Modify the config struct directly
		log.Debugf("Overriding config GetPruned with flag: %t", prunedFlag)
	}
	if cmd.Flags().Changed("fp16") {
		fp16Flag, _ := cmd.Flags().GetBool("fp16")
		cfg.GetFp16 = fp16Flag // Modify the config struct directly
		log.Debugf("Overriding config GetFp16 with flag: %t", fp16Flag)
	}

	log.WithField("params", fmt.Sprintf("%+v", params)).Debug("Final query parameters set")
	return params
}

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

// downloadWorker handles the actual download of a file and updates the database.
func downloadWorker(id int, jobs <-chan downloadJob, db *database.DB, fileDownloader *downloader.Downloader, wg *sync.WaitGroup, writer *uilive.Writer) {
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
		if globalConfig.SaveMetadata && finalStatus == models.StatusDownloaded { // Only save metadata on success
			if err := saveMetadataFile(pd, finalPath); err != nil {
				// Error already logged by saveMetadataFile
				fmt.Fprintf(writer.Newline(), "Worker %d: Error saving metadata for %s: %v\n", id, filepath.Base(finalPath), err)
			}
		} else if globalConfig.SaveMetadata && finalStatus != models.StatusDownloaded {
			log.Debugf("Worker %d: Skipping metadata save for %s due to download failure.", id, pd.TargetFilepath)
		}
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

// saveModelInfoFile saves the full model metadata to a .json file.
// It saves the file to basePath/model_info/[model.ID].json.
func saveModelInfoFile(model models.Model, basePath string) error {
	// Construct the directory path
	infoDirPath := filepath.Join(basePath, "model_info")

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

	// =============================================
	// Phase 1: Metadata Gathering & Filtering
	// =============================================
	log.Info("--- Starting Phase 1: Metadata Gathering & DB Check ---")
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
	nextCursor := ""
	pageCount := 0 // For logging request number

	var downloadsToQueue []potentialDownload // Holds downloads confirmed for queueing after DB check
	var totalQueuedSizeBytes uint64 = 0      // Track size of queued downloads only
	var loopErr error                        // Store loop errors

	// Loop through API pages using cursor
	for {
		pageCount++
		currentParams := queryParams
		if nextCursor != "" {
			// Use the Cursor field added to QueryParameters
			currentParams.Cursor = nextCursor
		}

		// Use the new ConstructApiUrl function
		apiURL := models.ConstructApiUrl(currentParams)
		log.Debugf("Requesting URL (Page %d): %s", pageCount, apiURL)

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			loopErr = fmt.Errorf("failed to create request (Page %d): %w", pageCount, err)
			break // Critical error, stop gathering
		}
		// Add API Key if available (use correct field name: ApiKey)
		if globalConfig.ApiKey != "" {
			req.Header.Add("Authorization", "Bearer "+globalConfig.ApiKey)
			log.Debug("Added API Key to request header.")
		} else {
			log.Warn("Civitai API Key not found in config. Requesting anonymously.")
		}

		resp, err := metadataClient.Do(req) // Use metadataClient here
		if err != nil {
			// Improved error handling for network issues
			if urlErr, ok := err.(*url.Error); ok && urlErr.Timeout() {
				log.WithError(err).Warnf("Timeout fetching metadata page %d. Retrying after delay...", pageCount)
				time.Sleep(5 * time.Second) // Simple retry delay
				// TODO: Implement retry logic instead of breaking immediately
				loopErr = fmt.Errorf("timeout fetching metadata page %d: %w", pageCount, err)
				break // Or implement retry logic
			}
			loopErr = fmt.Errorf("failed to fetch metadata (Page %d): %w", pageCount, err)
			break // Critical error, stop gathering
		}

		// Ensure body is read and closed regardless of status code
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.WithError(closeErr).Warn("Error closing response body after reading")
		}

		if readErr != nil {
			log.WithError(readErr).Errorf("Error reading response body for page %d, status %s", pageCount, resp.Status)
			// Treat as a failed request for this page
			loopErr = fmt.Errorf("failed to read response body (Page %d): %w", pageCount, readErr)
			// Decide whether to break or try next page (if cursor exists)
			// Breaking is safer for now.
			break
		}

		// Handle non-200 status codes after reading body
		if resp.StatusCode != http.StatusOK {
			errMsg := fmt.Sprintf("API request failed (Page %d) with status %s", pageCount, resp.Status)
			if len(bodyBytes) > 0 {
				// Limit error message length from body
				maxLen := 200
				bodyStr := string(bodyBytes)
				if len(bodyStr) > maxLen {
					bodyStr = bodyStr[:maxLen] + "..."
				}
				errMsg += fmt.Sprintf(". Response: %s", bodyStr)
			}
			log.Error(errMsg) // Log non-OK statuses as errors

			// Specific handling for common errors (use correct field name: ApiKey)
			if resp.StatusCode == http.StatusUnauthorized && globalConfig.ApiKey != "" {
				errMsg += ". Check if your Civitai API Key is correct/valid."
				log.Error(errMsg) // Log specific guidance
			} else if resp.StatusCode == http.StatusTooManyRequests {
				errMsg += ". Rate limited. Applying longer delay..."
				log.Warn(errMsg) // Log rate limit as warning
				delay := time.Duration(globalConfig.ApiDelayMs) * time.Millisecond * 5
				if delay == 0 {
					delay = 10 * time.Second // Ensure a minimum delay if config is 0
				}
				log.Warnf("Applying rate limit delay: %v", delay)
				time.Sleep(delay)
				// TODO: Retry logic would be better here. For now, just breaking.
			}
			loopErr = fmt.Errorf(errMsg) // Set loopErr for any non-OK status
			break                        // Stop processing on any API error for now
		}

		// Decode JSON response (use renamed ApiResponse struct)
		var response models.ApiResponse
		if err := json.Unmarshal(bodyBytes, &response); err != nil {
			loopErr = fmt.Errorf("failed to decode API response (Page %d): %w", pageCount, err)
			log.WithError(err).Errorf("Response body sample: %s", string(bodyBytes[:min(len(bodyBytes), 200)])) // Log sample on decode error
			break                                                                                               // Stop if response is unparseable
		}

		// Extract next cursor *before* processing items
		// Check Metadata pointer before dereferencing
		if response.Metadata.NextCursor != "" {
			nextCursor = response.Metadata.NextCursor
			log.Debugf("API Metadata: TotalItems=%d, CurrentPage=%d, PageSize=%d, NextCursor=%s",
				response.Metadata.TotalItems, response.Metadata.CurrentPage, response.Metadata.PageSize, response.Metadata.NextCursor)
		} else {
			log.Warn("API response missing next cursor.")
			nextCursor = "" // Ensure cursor logic stops
		}

		// --- Process Models from this Page ---
		var potentialDownloadsThisPage []potentialDownload // Temporary list for this page

		log.Debugf("Processing %d models from request %d for potential downloads...", len(response.Items), pageCount)
		// --- model processing loop ---
		for _, model := range response.Items {
			// --- Save Full Model Info if Flag is Set ---
			saveFullInfo, _ := cmd.Flags().GetBool("save-model-info")
			if saveFullInfo {
				if err := saveModelInfoFile(model, globalConfig.SavePath); err != nil {
					// Log error but continue processing other models
					log.WithError(err).Warnf("Failed to save full model info for model %d (%s)", model.ID, model.Name)
				}
			}
			// --- End Save Full Model Info ---

			// --- Version Selection ---
			latestVersion := models.ModelVersion{}
			latestTime := time.Time{}
			if len(model.ModelVersions) > 0 {
				for _, version := range model.ModelVersions {
					if version.PublishedAt == "" {
						log.Warnf("Skipping version %s in model %s (%d): PublishedAt timestamp is empty.", version.Name, model.Name, model.ID)
						continue
					}
					publishedAt, errParse := time.Parse(time.RFC3339Nano, version.PublishedAt)
					if errParse != nil {
						publishedAt, errParse = time.Parse(time.RFC3339, version.PublishedAt)
						if errParse != nil {
							log.WithError(errParse).Warnf("Skipping version %s in model %s (%d): Error parsing time '%s'", version.Name, model.Name, model.ID, version.PublishedAt)
							continue
						}
					}
					if latestVersion.ID == 0 || publishedAt.After(latestTime) {
						latestTime = publishedAt
						latestVersion = version
					}
				}
			}
			if latestVersion.ID == 0 {
				log.Debugf("Skipping model %s (%d) - no valid versions found.", model.Name, model.ID)
				continue
			}
			// --- End Version Selection ---

			// --- Filtering Logic (Model Level) ---
			if len(globalConfig.IgnoreBaseModels) > 0 { // Check if slice is non-empty
				ignore := false
				for _, ignoreBaseModel := range globalConfig.IgnoreBaseModels {
					if strings.Contains(strings.ToLower(latestVersion.BaseModel), strings.ToLower(ignoreBaseModel)) {
						log.Debugf("Skipping model %s (%d, version %s) due to ignored base model '%s'.", model.Name, model.ID, latestVersion.Name, ignoreBaseModel)
						ignore = true
						break
					}
				}
				if ignore {
					continue
				}
			}
			// --- End Filtering Logic ---

			// Clean version data once per model version (for metadata saving)
			versionWithoutFilesImages := latestVersion // Make a copy
			versionWithoutFilesImages.Files = nil      // Clear files slice
			versionWithoutFilesImages.Images = nil     // Clear images slice

			// Iterate files to find potential downloads
		fileLoop:
			for _, file := range latestVersion.Files {
				// --- Filtering Logic (File Level) ---
				if file.Hashes.CRC32 == "" {
					log.Debugf("Skipping file %s in model %s (%d): Missing CRC32 hash.", file.Name, model.Name, model.ID)
					continue
				} // Need hash for DB key

				// Apply filters from config *before* constructing path/checking DB
				if globalConfig.GetOnlyPrimaryModel && !file.Primary {
					log.Debugf("Skipping non-primary file %s in model %s (%d).", file.Name, model.Name, model.ID)
					continue
				}
				// REMOVED Metadata nil check - structs cannot be nil
				// Ensure Metadata Format is not empty
				if file.Metadata.Format == "" {
					log.Debugf("Skipping file %s in model %s (%d): Missing metadata format.", file.Name, model.Name, model.ID)
					continue
				}
				if strings.ToLower(file.Metadata.Format) != "safetensor" {
					log.Debugf("Skipping non-safetensor file %s (Format: %s) in model %s (%d).", file.Name, file.Metadata.Format, model.Name, model.ID)
					continue
				}
				// Checkpoint specific filters (logic remains the same)
				if strings.EqualFold(model.Type, "checkpoint") {
					sizeStr := fmt.Sprintf("%v", file.Metadata.Size) // Handle potential non-string types
					fpStr := fmt.Sprintf("%v", file.Metadata.Fp)     // Handle potential non-string types

					if globalConfig.GetPruned && !strings.EqualFold(sizeStr, "pruned") {
						log.Debugf("Skipping non-pruned file %s (Size: %s) in checkpoint model %s (%d).", file.Name, sizeStr, model.Name, model.ID)
						continue
					}
					if globalConfig.GetFp16 && !strings.EqualFold(fpStr, "fp16") {
						log.Debugf("Skipping non-fp16 file %s (FP: %s) in checkpoint model %s (%d).", file.Name, fpStr, model.Name, model.ID)
						continue
					}
				}
				// Ignore filenames containing specific strings (logic remains the same)
				if len(globalConfig.IgnoreFileNameStrings) > 0 { // Check if slice is non-empty
					for _, ignoreFileName := range globalConfig.IgnoreFileNameStrings {
						if strings.Contains(strings.ToLower(file.Name), strings.ToLower(ignoreFileName)) {
							log.Debugf("Skipping file %s in model %s (%d) due to ignored filename string '%s'.", file.Name, model.Name, model.ID, ignoreFileName)
							continue fileLoop
						}
					}
				}
				// --- End Filtering Logic ---

				// --- Path/Filename Construction --- (logic remains the same)
				var slug string
				modelTypeName := helpers.ConvertToSlug(model.Type)
				// Use "unknown-base" if base model is empty
				baseModelStr := latestVersion.BaseModel
				if baseModelStr == "" {
					baseModelStr = "unknown-base"
				}
				baseModelSlug := helpers.ConvertToSlug(baseModelStr)
				modelNameSlug := helpers.ConvertToSlug(model.Name)
				// Basic structure: type-base/model_name OR base/model_name for checkpoints
				if !strings.EqualFold(model.Type, "checkpoint") {
					slug = filepath.Join(modelTypeName+"-"+baseModelSlug, modelNameSlug)
				} else {
					slug = filepath.Join(baseModelSlug, modelNameSlug) // Checkpoints often grouped by base model
				}

				// Construct filename: original_slugified-CRC32[-fp][-size].ext
				baseFileName := helpers.ConvertToSlug(file.Name)
				ext := filepath.Ext(baseFileName)                    // Get original extension
				baseFileName = strings.TrimSuffix(baseFileName, ext) // Remove extension

				// Correct extension if format is safetensor but ext isn't
				if strings.ToLower(file.Metadata.Format) == "safetensor" && !strings.EqualFold(ext, ".safetensors") {
					ext = ".safetensors"
				}
				if ext == "" { // Ensure there's an extension
					ext = ".bin" // Default extension if none found
					log.Warnf("File %s in model %s (%d) has no extension, defaulting to '.bin'", file.Name, model.Name, model.ID)
				}

				// Store the intended final base filename (without metadata suffix)
				finalBaseFilenameOnly := baseFileName + ext

				// --- Build Metadata Suffix ---
				dbKeySimple := strings.ToUpper(file.Hashes.CRC32) // Just the hash for suffix
				metaSuffixParts := []string{dbKeySimple}
				if strings.EqualFold(model.Type, "checkpoint") {
					if fpStr := fmt.Sprintf("%v", file.Metadata.Fp); fpStr != "" {
						metaSuffixParts = append(metaSuffixParts, helpers.ConvertToSlug(fpStr))
					}
					if sizeStr := fmt.Sprintf("%v", file.Metadata.Size); sizeStr != "" {
						metaSuffixParts = append(metaSuffixParts, helpers.ConvertToSlug(sizeStr))
					}
				}
				metaSuffix := "-" + strings.Join(metaSuffixParts, "-")
				// --- End Build Metadata Suffix ---

				constructedFileNameWithSuffix := baseFileName + metaSuffix + ext
				// Use SavePath from globalConfig for the absolute path
				fullDirPath := filepath.Join(globalConfig.SavePath, slug)
				fullFilePath := filepath.Join(fullDirPath, constructedFileNameWithSuffix)
				// --- End Path/Filename Construction ---

				// Passed filters, create potential download entry FOR THIS PAGE
				pd := potentialDownload{
					ModelName:         model.Name,
					ModelType:         model.Type,
					VersionName:       latestVersion.Name,
					BaseModel:         latestVersion.BaseModel, // Store original base model string
					Creator:           model.Creator,
					File:              file, // Contains URL, Hashes, SizeKB etc.
					ModelVersionID:    latestVersion.ID,
					TargetFilepath:    fullFilePath, // This path includes the metadata suffix
					Slug:              slug,
					FinalBaseFilename: finalBaseFilenameOnly,     // Store the base filename WITHOUT suffix
					CleanedVersion:    versionWithoutFilesImages, // Store cleaned version for metadata
				}
				potentialDownloadsThisPage = append(potentialDownloadsThisPage, pd)
				log.Debugf("Passed filters: %s (Model: %s (%d)) -> %s", file.Name, model.Name, model.ID, fullFilePath)
			} // End file loop
		} // End model loop for this page
		// --- end model processing loop ---

		// --- Process this page's potential downloads against the DB ---
		log.Debugf("Checking %d potential downloads from page %d against database...", len(potentialDownloadsThisPage), pageCount)
		// Pass address of globalConfig
		queuedFromPage, sizeFromPage := processPage(db, potentialDownloadsThisPage, &globalConfig)
		if len(queuedFromPage) > 0 {
			downloadsToQueue = append(downloadsToQueue, queuedFromPage...)
			totalQueuedSizeBytes += sizeFromPage
			log.Infof("Queued %d file(s) (Size: %s) from page %d after DB check.", len(queuedFromPage), helpers.BytesToSize(sizeFromPage), pageCount)
		} else {
			log.Debugf("No new files queued from page %d after DB check.", pageCount)
		}

		// Check if we should stop (no next cursor)
		if nextCursor == "" {
			log.Info("Finished gathering metadata: No next cursor provided by API.")
			break // Exit the loop normally
		}

		// --- Apply Polite Delay ---
		if globalConfig.ApiDelayMs > 0 {
			log.Debugf("Applying API delay: %d ms", globalConfig.ApiDelayMs)
			time.Sleep(time.Duration(globalConfig.ApiDelayMs) * time.Millisecond)
		}
	} // End API cursor loop

	if loopErr != nil {
		log.Errorf("Metadata gathering phase finished with error: %v", loopErr)
		// Exit if there was a critical error during gathering
		log.Error("Aborting due to error during metadata gathering.")
		return
	}
	log.Info("--- Finished Phase 1: Metadata Gathering & DB Check --- ")

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
	for w := 1; w <= concurrencyLevel; w++ {
		wg.Add(1)
		// Pass db, fileDownloader, wg, and writer to the worker
		go downloadWorker(w, jobs, db, fileDownloader, &wg, writer)
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

// Helper function for min(a, b)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
