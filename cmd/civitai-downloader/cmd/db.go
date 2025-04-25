package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/helpers"
	"go-civitai-download/internal/models"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// dbCmd represents the base command for database operations
var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Interact with the download database",
	Long:  `Perform operations like viewing, verifying, or managing entries in the download database.`,
	// No Run function for the base db command itself
}

// dbViewCmd represents the command to view database entries
var dbViewCmd = &cobra.Command{
	Use:   "view",
	Short: "View entries stored in the database",
	Long:  `Lists the models and files that have been recorded in the database.`,
	Run:   runDbView,
}

// dbVerifyCmd represents the command to verify database entries against the filesystem
var dbVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify database entries against the filesystem",
	Long: `Checks if the files listed in the database exist at their expected locations 
and optionally verifies their hashes.`,
	Run: runDbVerify,
}

// dbRedownloadCmd represents the command to redownload a file based on its DB key
var dbRedownloadCmd = &cobra.Command{
	Use:   "redownload [MODEL_VERSION_ID]",
	Short: "Redownload a file stored in the database based on Model Version ID",
	Long: `Attempts to redownload a specific file using the information stored
in the database entry identified by the provided Model Version ID (used as the database key).`,
	Args: cobra.ExactArgs(1), // Requires exactly one argument (the version ID)
	Run:  runDbRedownload,
}

// dbSearchCmd represents the command to search database entries by model name
var dbSearchCmd = &cobra.Command{
	Use:   "search [MODEL_NAME_QUERY]",
	Short: "Search database entries by model name",
	Long: `Searches database entries for models whose names contain the provided query text (case-insensitive).
Prints matching entries.`,
	Args: cobra.ExactArgs(1), // Requires exactly one argument
	Run:  runDbSearch,
}

func init() {
	rootCmd.AddCommand(dbCmd)
	dbCmd.AddCommand(dbViewCmd)
	dbCmd.AddCommand(dbVerifyCmd)
	dbCmd.AddCommand(dbRedownloadCmd) // Add the redownload command
	dbCmd.AddCommand(dbSearchCmd)     // Add the search command

	// Add flags specific to db view if needed (e.g., filtering)
	// dbViewCmd.Flags().StringP("filter", "f", "", "Filter results (e.g., by model name)")

	// Add flags specific to db verify
	dbVerifyCmd.Flags().Bool("check-hash", true, "Perform hash check for existing files")

	// Add flags specific to db redownload if needed (e.g., force overwrite without hash check?)
	// dbRedownloadCmd.Flags().Bool("force", false, "Force redownload even if file exists and hash matches")
}

func runDbView(cmd *cobra.Command, args []string) {
	log.Info("Viewing database entries...")

	// Use globalConfig loaded by PersistentPreRunE
	if globalConfig.DatabasePath == "" {
		log.Fatal("Database path is not set in the configuration. Please check config file or path.")
	}

	// Open Database using globalConfig
	db, err := database.Open(globalConfig.DatabasePath)
	if err != nil {
		log.WithError(err).Fatalf("Failed to open database at %s", globalConfig.DatabasePath)
	}
	defer db.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0) // Adjust padding and alignment
	fmt.Fprintln(tw, "Model Name\tVersion Name\tFilename\tFolder\tType\tBase Model\tCreator\tStatus\tDB Key (VersionID)")
	fmt.Fprintln(tw, "----------\t------------\t--------\t------\t----\t----------\t-------\t------\t------------------")

	count := 0
	// Use Fold to iterate over key-value pairs
	errFold := db.Fold(func(key []byte, value []byte) error {
		keyStr := string(key)
		// Skip internal keys like page state
		if !strings.HasPrefix(keyStr, "v_") { // Only process keys starting with "v_"
			return nil
		}

		// Value is already provided by Fold, no need for db.Get
		var entry models.DatabaseEntry
		err := json.Unmarshal(value, &entry)
		if err != nil {
			log.WithError(err).Warnf("Failed to unmarshal JSON for key %s: %s", keyStr, string(value))
			return nil // Continue folding over other keys
		}

		// Print table row using the added fields, including Status
		// Extract version ID from key for display
		versionIDStr := strings.TrimPrefix(keyStr, "v_")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			entry.ModelName, // Use added ModelName
			entry.Version.Name,
			entry.Filename,
			entry.Folder,
			entry.ModelType, // Use added ModelType
			entry.Version.BaseModel,
			entry.Creator.Username, // Print the username from the Creator struct
			entry.Status,           // Added Status field
			versionIDStr,           // Display the version ID
		)
		count++
		return nil
	})

	if errFold != nil {
		log.WithError(errFold).Error("Error occurred during database scan (Fold)")
	}

	if err := tw.Flush(); err != nil {
		log.WithError(err).Error("Error flushing table writer for db view")
	}
	log.Infof("Displayed %d entries.", count)
}

func runDbVerify(cmd *cobra.Command, args []string) {
	log.Info("Verifying database entries against filesystem...")
	checkHashFlag, _ := cmd.Flags().GetBool("check-hash")

	// Use globalConfig loaded by PersistentPreRunE
	if globalConfig.DatabasePath == "" {
		log.Fatal("Database path is not set in the configuration. Please check config file or path.")
	}
	if globalConfig.SavePath == "" {
		log.Fatal("Save path is not set in the configuration. Please check config file or path.")
	}

	// Open Database using globalConfig
	db, err := database.Open(globalConfig.DatabasePath)
	if err != nil {
		log.WithError(err).Fatalf("Failed to open database at %s", globalConfig.DatabasePath)
	}
	defer db.Close()

	var totalEntries, foundOk, foundHashMismatch, missing int

	log.Info("Attempting to list keys using db.Keys()...")
	keysChan := db.Keys()
	keyCount := 0
	for key := range keysChan {
		keyCount++
		keyStr := string(key)
		log.Debugf("[Keys()] Found DB key: %s", keyStr)

		// Skip non-version keys
		if !strings.HasPrefix(keyStr, "v_") {
			log.Debugf("Skipping non-version key: %s", keyStr)
			continue // Skip to the next key
		}

		// Increment total counter for actual entries processed
		totalEntries++

		// Get the value for the key
		value, errGet := db.Get(key)
		if errGet != nil {
			log.WithError(errGet).Warnf("Failed to get value for key %s, skipping verification.", keyStr)
			continue // Skip to the next key
		}

		var entry models.DatabaseEntry
		err := json.Unmarshal(value, &entry)
		if err != nil {
			log.WithError(err).Warnf("Failed to unmarshal JSON for key %s, skipping verification for this entry.", keyStr)
			continue // Skip to the next key
		}

		// Construct the expected full path using globalConfig
		expectedPath := filepath.Join(globalConfig.SavePath, entry.Folder, entry.Filename)

		// --- Check Main Model File ---
		mainFileFound := false // Track if the main file exists
		if _, err := os.Stat(expectedPath); err == nil {
			mainFileFound = true
			// File exists, optionally check hash
			if checkHashFlag {
				// Pass the correct hash struct
				if helpers.CheckHash(expectedPath, entry.File.Hashes) {
					// Include status in log
					log.WithFields(log.Fields{"path": expectedPath, "status": entry.Status}).Info("[OK] File exists and hash matches.")
					foundOk++
				} else {
					// Include status in log
					log.WithFields(log.Fields{"path": expectedPath, "status": entry.Status}).Warn("[MISMATCH] File exists but hash mismatch.")
					foundHashMismatch++
				}
			} else {
				// Include status in log
				log.WithFields(log.Fields{"path": expectedPath, "status": entry.Status}).Info("[FOUND] File exists (hash check skipped).")
				foundOk++ // Count as OK if hash check is skipped
			}
		} else if os.IsNotExist(err) {
			// File does not exist
			mainFileFound = false
			// Include status in log
			log.WithFields(log.Fields{"path": expectedPath, "status": entry.Status}).Error("[MISSING] File not found.")
			missing++
		} else {
			// Other error stating the file (e.g., permission issues)
			mainFileFound = false // Treat as not found for metadata check
			log.WithError(err).Errorf("[ERROR] Could not check file status for %s", expectedPath)
		}
		// --- End Check Main Model File ---

		// --- Check/Create Metadata File if Enabled ---
		// Only proceed if SaveMetadata is true AND the main file was found
		if globalConfig.SaveMetadata && mainFileFound {
			// Construct metadata filepath
			baseFilename := strings.TrimSuffix(entry.Filename, filepath.Ext(entry.Filename))
			metaFilename := baseFilename + ".json"
			metaFilepath := filepath.Join(globalConfig.SavePath, entry.Folder, metaFilename)

			if _, metaStatErr := os.Stat(metaFilepath); metaStatErr != nil {
				if os.IsNotExist(metaStatErr) {
					// Metadata file missing, attempt to create it
					log.WithField("path", metaFilepath).Warn("[METADATA MISSING] Creating metadata file...")

					// Ensure directory exists BEFORE writing
					metaDir := filepath.Dir(metaFilepath)
					if mkdirErr := os.MkdirAll(metaDir, 0700); mkdirErr != nil {
						log.WithError(mkdirErr).Errorf("Failed to create directory for metadata file %s", metaFilepath)
						continue // Skip metadata creation if dir fails
					}

					// Need to marshal the correct structure (entry.Version, not entire entry)
					versionToSave := entry.Version // Make a copy
					versionToSave.Files = nil      // Clear files/images for metadata
					versionToSave.Images = nil
					jsonData, jsonErr := json.MarshalIndent(versionToSave, "", "  ")
					if jsonErr != nil {
						log.WithError(jsonErr).Errorf("Failed to marshal metadata for %s", metaFilename)
					} else {
						if writeErr := os.WriteFile(metaFilepath, jsonData, 0600); writeErr != nil {
							log.WithError(writeErr).Errorf("Failed to write metadata file %s", metaFilepath)
						} else {
							log.WithField("path", metaFilepath).Info("[METADATA CREATED] Successfully wrote metadata file.")
						}
					}
				} else {
					// Other error stating the metadata file
					log.WithError(metaStatErr).Errorf("[METADATA ERROR] Could not check metadata file status for %s", metaFilepath)
				}
			} else {
				// Metadata file exists
				log.WithField("path", metaFilepath).Info("[METADATA OK] Metadata file exists.")
			}
		} else if globalConfig.SaveMetadata && !mainFileFound {
			// Log skipping metadata check because main file is missing
			metaFilename := strings.TrimSuffix(entry.Filename, filepath.Ext(entry.Filename)) + ".json"
			metaFilepath := filepath.Join(globalConfig.SavePath, entry.Folder, metaFilename)
			log.WithField("path", metaFilepath).Debug("[METADATA SKIP] Skipping metadata check/creation because main file is missing.")
		}
		// --- End Check/Create Metadata File ---
	} // End of loop over keysChan

	log.Infof("db.Keys() finished processing. Found %d raw keys.", keyCount)

	log.Infof("Verification Summary: Total Entries=%d, OK=%d, Missing=%d, Mismatch=%d",
		totalEntries, foundOk, missing, foundHashMismatch)
}

func runDbRedownload(cmd *cobra.Command, args []string) {
	versionIDStr := args[0]
	log.Infof("Attempting to redownload file with Model Version ID: %s", versionIDStr)

	// Use globalConfig loaded by PersistentPreRunE
	if globalConfig.DatabasePath == "" {
		log.Fatal("Database path is not set in the configuration. Please check config file or path.")
	}
	if globalConfig.SavePath == "" {
		log.Fatal("Save path is not set in the configuration. Please check config file or path.")
	}

	// Open Database using globalConfig
	db, err := database.Open(globalConfig.DatabasePath)
	if err != nil {
		log.WithError(err).Fatalf("Failed to open database at %s", globalConfig.DatabasePath)
	}
	defer db.Close()

	// Construct the database key
	dbKey := fmt.Sprintf("v_%s", versionIDStr)

	// Get the database entry
	value, err := db.Get([]byte(dbKey))
	if errors.Is(err, database.ErrNotFound) {
		log.Fatalf("No database entry found for Model Version ID %s (Key: %s)", versionIDStr, dbKey)
	} else if err != nil {
		log.WithError(err).Fatalf("Failed to retrieve database entry for key %s", dbKey)
	}

	var entry models.DatabaseEntry
	err = json.Unmarshal(value, &entry)
	if err != nil {
		log.WithError(err).Fatalf("Failed to unmarshal database entry for key %s", dbKey)
	}

	// Reconstruct the expected full path using globalConfig
	expectedPath := filepath.Join(globalConfig.SavePath, entry.Folder, entry.Filename)
	log.Infof("Target path for redownload: %s", expectedPath)
	log.Infof("Download URL from DB: %s", entry.File.DownloadUrl)

	// Ensure target directory exists
	if !helpers.CheckAndMakeDir(filepath.Dir(expectedPath)) {
		log.Fatalf("Failed to ensure directory exists: %s", filepath.Dir(expectedPath))
	}

	// Initialize downloader using the helper function from download.go
	log.Debug("Initializing HTTP client for redownload...")
	// createDownloaderClient is in download.go, cannot be called directly here.
	// Create a new client instance for this command.
	// TODO: Refactor client creation/sharing?
	downloaderHttpClient := &http.Client{Timeout: 30 * time.Minute} // Longer timeout for downloads
	fileDownloader := downloader.NewDownloader(downloaderHttpClient, globalConfig.ApiKey)

	// Perform the download, checking the error
	// Pass the Model Version ID from the database entry
	finalPath, err := fileDownloader.DownloadFile(expectedPath, entry.File.DownloadUrl, entry.File.Hashes, entry.Version.ID)

	if err == nil {
		log.Infof("Successfully redownloaded and verified: %s", finalPath)
	} else {
		// Log specific errors
		logEntry := log.WithFields(log.Fields{
			"key": dbKey,
			"url": entry.File.DownloadUrl,
		})
		if errors.Is(err, downloader.ErrHashMismatch) {
			logEntry.WithError(err).Error("Redownload failed: Hash mismatch after download.")
		} else if errors.Is(err, downloader.ErrHttpStatus) {
			logEntry.WithError(err).Error("Redownload failed: Unexpected HTTP status.")
		} else if errors.Is(err, downloader.ErrFileSystem) {
			logEntry.WithError(err).Error("Redownload failed: Filesystem error.")
		} else if errors.Is(err, downloader.ErrHttpRequest) {
			logEntry.WithError(err).Error("Redownload failed: HTTP request error.")
		} else {
			logEntry.WithError(err).Errorf("Redownload failed for an unknown reason.")
		}
		// Consider exiting with non-zero status code on failure
		os.Exit(1)
	}
}

func runDbSearch(cmd *cobra.Command, args []string) {
	searchTerm := strings.ToLower(args[0]) // Case-insensitive search
	log.Infof("Searching database entries for model name containing: '%s'", searchTerm)

	// Use globalConfig loaded by PersistentPreRunE
	if globalConfig.DatabasePath == "" {
		log.Fatal("Database path is not set in the configuration.")
	}

	db, err := database.Open(globalConfig.DatabasePath)
	if err != nil {
		log.WithError(err).Fatalf("Failed to open database at %s", globalConfig.DatabasePath)
	}
	defer db.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Model Name\tVersion Name\tFilename\tFolder\tType\tBase Model\tCreator\tStatus\tDB Key (VersionID)")
	fmt.Fprintln(tw, "----------\t------------\t--------\t------\t----\t----------\t-------\t------\t------------------")

	matchCount := 0
	errFold := db.Fold(func(key []byte, value []byte) error {
		keyStr := string(key)
		// Skip non-version keys
		if !strings.HasPrefix(keyStr, "v_") {
			return nil
		}

		var entry models.DatabaseEntry
		err := json.Unmarshal(value, &entry)
		if err != nil {
			log.WithError(err).Warnf("Failed to unmarshal JSON for key %s, skipping search check.", keyStr)
			return nil
		}

		// Perform case-insensitive substring search
		if strings.Contains(strings.ToLower(entry.ModelName), searchTerm) {
			matchCount++
			// Extract version ID from key for display
			versionIDStr := strings.TrimPrefix(keyStr, "v_")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				entry.ModelName,
				entry.Version.Name,
				entry.Filename,
				entry.Folder,
				entry.ModelType,
				entry.Version.BaseModel,
				entry.Creator.Username,
				entry.Status, // Added Status field
				versionIDStr, // Display the version ID
			)
		}
		return nil
	})

	if errFold != nil {
		log.WithError(errFold).Error("Error occurred during database scan (Fold)")
	}

	if err := tw.Flush(); err != nil {
		log.WithError(err).Error("Error flushing table writer for db search")
	}
	log.Infof("Found %d matching entries for query '%s'.", matchCount, searchTerm)
}
