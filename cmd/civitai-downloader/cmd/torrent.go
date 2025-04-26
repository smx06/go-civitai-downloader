package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/models"
)

// Struct to hold job parameters for torrent workers
type torrentJob struct {
	SourcePath     string
	Trackers       []string
	OutputDir      string
	Overwrite      bool
	GenerateMagnet bool
	LogFields      log.Fields // For context in worker logs
}

// torrentWorker function
func torrentWorker(id int, jobs <-chan torrentJob, wg *sync.WaitGroup, successCounter *atomic.Int64, failureCounter *atomic.Int64) {
	defer wg.Done()
	log.Debugf("Torrent Worker %d starting", id)
	for job := range jobs {
		log.WithFields(job.LogFields).Infof("Worker %d: Processing torrent job for directory %s", id, job.SourcePath)
		err := generateTorrentFile(job.SourcePath, job.Trackers, job.OutputDir, job.Overwrite, job.GenerateMagnet)
		if err != nil {
			log.WithFields(job.LogFields).WithError(err).Errorf("Worker %d: Failed to generate torrent for %s", id, job.SourcePath)
			failureCounter.Add(1)
		} else {
			log.WithFields(job.LogFields).Infof("Worker %d: Successfully generated torrent for %s", id, job.SourcePath)
			successCounter.Add(1)
		}
	}
	log.Debugf("Torrent Worker %d finished", id)
}

var (
	torrentModelIDs     []int
	announceURLs        []string
	torrentOutputDir    string
	overwriteTorrents   bool
	generateMagnetLinks bool
)

var torrentCmd = &cobra.Command{
	Use:   "torrent",
	Short: "Generate .torrent files for downloaded models",
	Long: `Generates BitTorrent metainfo (.torrent) files for models previously downloaded
using the 'download' command. Requires access to the download history database
and the downloaded files themselves. You must specify tracker announce URLs.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(announceURLs) == 0 {
			return errors.New("at least one --announce URL is required")
		}

		// Get concurrency level
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		if concurrency <= 0 {
			log.Warnf("Invalid concurrency value %d, defaulting to 4", concurrency)
			concurrency = 4
		}

		if globalConfig.SavePath == "" {
			log.Error("Save path is not configured (--save-path or config file)")
			return errors.New("save path is not configured (--save-path or config file)")
		}

		db, err := database.Open(globalConfig.DatabasePath)
		if err != nil {
			log.WithError(err).Errorf("Error opening database at %s", globalConfig.DatabasePath)
			return fmt.Errorf("error opening database: %w", err)
		}
		defer db.Close()

		targetDownloads := []models.DatabaseEntry{}
		modelIDSet := make(map[int]struct{})
		if len(torrentModelIDs) > 0 {
			for _, id := range torrentModelIDs {
				modelIDSet[id] = struct{}{}
			}
		}

		log.Info("Scanning database for model entries...")
		errFold := db.Fold(func(key []byte, value []byte) error {
			keyStr := string(key)
			if !strings.HasPrefix(keyStr, "v_") {
				return nil
			}

			var entry models.DatabaseEntry
			if err := json.Unmarshal(value, &entry); err != nil {
				log.WithError(err).Warnf("Failed to unmarshal JSON for key %s, skipping", keyStr)
				return nil
			}

			if entry.Folder == "" || entry.Filename == "" {
				log.WithFields(log.Fields{
					"modelID":   entry.Version.ModelId,
					"versionID": entry.Version.ID,
				}).Warn("Skipping entry due to missing Folder or Filename.")
				return nil
			}

			if len(torrentModelIDs) > 0 {
				if _, exists := modelIDSet[entry.Version.ModelId]; exists {
					targetDownloads = append(targetDownloads, entry)
				}
			} else {
				targetDownloads = append(targetDownloads, entry)
			}
			return nil
		})

		if errFold != nil {
			log.WithError(errFold).Error("Error scanning database")
			return fmt.Errorf("error scanning database: %w", errFold)
		}

		if len(targetDownloads) == 0 {
			if len(torrentModelIDs) > 0 {
				log.Warnf("No downloaded models found matching IDs: %v", torrentModelIDs)
			} else {
				log.Info("No download entries found in the database.")
			}
			return nil
		}

		log.Infof("Generating torrents for %d model directories using %d workers...", len(targetDownloads), concurrency)

		// --- Worker Pool Setup ---
		jobs := make(chan torrentJob, concurrency) // Buffered channel
		var wg sync.WaitGroup
		var successCounter atomic.Int64
		var failureCounter atomic.Int64

		// Start workers
		for i := 1; i <= concurrency; i++ {
			wg.Add(1)
			go torrentWorker(i, jobs, &wg, &successCounter, &failureCounter)
		}

		// --- Queue Jobs ---
		queuedJobs := 0
		skippedJobs := 0
		processedDirs := make(map[string]bool) // Track processed directories to avoid duplicates

		for _, dl := range targetDownloads {
			// Determine the directory path for this download entry
			dirPath := filepath.Join(globalConfig.SavePath, dl.Folder)

			// Check if this directory has already been queued
			if processedDirs[dirPath] {
				log.Debugf("Directory %s already queued for torrent generation, skipping duplicate entry (ModelID: %d, VersionID: %d)", dirPath, dl.Version.ModelId, dl.Version.ID)
				skippedJobs++
				continue
			}

			// Mark directory as processed
			processedDirs[dirPath] = true

			// Create and send job
			job := torrentJob{
				SourcePath:     dirPath,
				Trackers:       announceURLs,
				OutputDir:      torrentOutputDir,
				Overwrite:      overwriteTorrents,
				GenerateMagnet: generateMagnetLinks,
				LogFields: log.Fields{ // Add context for worker logs
					"modelID":   dl.Version.ModelId,
					"versionID": dl.Version.ID, // Use the first version ID encountered for this dir
					"directory": dirPath,
				},
			}
			jobs <- job
			queuedJobs++
		}

		close(jobs) // Signal no more jobs
		log.Infof("Queued %d unique directory jobs for torrent generation (%d entries skipped as duplicates). Waiting for workers...", queuedJobs, skippedJobs)

		// --- Wait for Workers ---
		wg.Wait()

		// --- Final Summary ---
		successCount := successCounter.Load()
		failCount := failureCounter.Load()

		log.Infof("Torrent generation complete. Success: %d, Failed: %d", successCount, failCount)
		if failCount > 0 {
			log.Errorf("%d torrents failed to generate", failCount)
			return fmt.Errorf("%d torrents failed to generate", failCount)
		}
		return nil
	},
}

// generateTorrentFile creates a .torrent file for the given sourcePath (file or directory).
// It can optionally also create a text file containing the magnet link.
func generateTorrentFile(sourcePath string, trackers []string, outputDir string, overwrite bool, generateMagnetLinks bool) error {
	stat, err := os.Stat(sourcePath)
	if os.IsNotExist(err) {
		log.WithField("path", sourcePath).Error("Source path not found for torrent generation")
		return fmt.Errorf("source path does not exist: %s", sourcePath)
	} else if err != nil {
		log.WithError(err).WithField("path", sourcePath).Error("Error stating source path")
		return fmt.Errorf("error stating source path %s: %w", sourcePath, err)
	} else if !stat.IsDir() {
		// Although the main loop now passes directories, we keep this check
		// in case the function is used differently elsewhere or for future robustness.
		log.WithField("path", sourcePath).Error("Source path is not a directory")
		return fmt.Errorf("source path is not a directory: %s", sourcePath)
	}

	// Use the directory name for the torrent file
	torrentFileName := fmt.Sprintf("%s.torrent", filepath.Base(sourcePath))
	var outPath string
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			log.WithError(err).WithField("dir", outputDir).Error("Error creating output directory")
			return fmt.Errorf("error creating output directory %s: %w", outputDir, err)
		}
		outPath = filepath.Join(outputDir, torrentFileName)
	} else {
		// Place the torrent file *inside* the source directory
		outPath = filepath.Join(sourcePath, torrentFileName)
	}

	if !overwrite {
		if _, err := os.Stat(outPath); err == nil {
			log.WithField("path", outPath).Info("Skipping existing torrent file (use --overwrite to replace)")
			return nil
		} else if !os.IsNotExist(err) {
			log.WithError(err).WithField("path", outPath).Warn("Could not check status of potential existing torrent file, attempting to overwrite")
		}
	} else {
		if _, err := os.Stat(outPath); err == nil {
			log.WithField("path", outPath).Warn("Overwriting existing torrent file")
		}
	}

	mi := metainfo.MetaInfo{
		AnnounceList: make([][]string, len(trackers)),
	}
	for i, tracker := range trackers {
		mi.AnnounceList[i] = []string{tracker}
	}
	if len(trackers) > 0 {
		mi.Announce = trackers[0]
	}

	mi.CreatedBy = "go-civitai-download"

	const pieceLength = 512 * 1024
	info := metainfo.Info{PieceLength: pieceLength}

	log.WithField("directory", sourcePath).Debug("Building torrent info...")
	err = info.BuildFromFilePath(sourcePath)
	if err != nil {
		log.WithError(err).WithField("path", sourcePath).Error("Error building torrent info from path")
		return fmt.Errorf("error building torrent info from path %s: %w", sourcePath, err)
	}
	mi.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		log.WithError(err).Error("Error marshaling torrent info")
		return fmt.Errorf("error marshaling torrent info: %w", err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		log.WithError(err).WithField("path", outPath).Error("Error creating torrent file")
		return fmt.Errorf("error creating torrent file %s: %w", outPath, err)
	}
	defer f.Close()

	err = mi.Write(f)
	if err != nil {
		log.WithError(err).WithField("path", outPath).Error("Error writing torrent file")
		return fmt.Errorf("error writing torrent file %s: %w", outPath, err)
	}

	log.WithField("path", outPath).Info("Successfully generated torrent file")

	if generateMagnetLinks {
		// Get the info hash directly from the MetaInfo struct
		infoHash := mi.HashInfoBytes()
		magnetParts := []string{
			fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash.HexString()),
			fmt.Sprintf("dn=%s", url.QueryEscape(stat.Name())), // Use directory name as display name
		}
		for _, tracker := range trackers {
			magnetParts = append(magnetParts, fmt.Sprintf("tr=%s", url.QueryEscape(tracker)))
		}
		magnetURI := strings.Join(magnetParts, "&")
		magnetFileName := fmt.Sprintf("%s-magnet.txt", strings.TrimSuffix(filepath.Base(outPath), filepath.Ext(outPath)))
		magnetOutPath := filepath.Join(filepath.Dir(outPath), magnetFileName)

		err = writeMagnetFile(magnetOutPath, magnetURI)
		if err != nil {
			// Log error but don't fail the whole torrent generation just for the magnet link
			log.WithError(err).WithField("path", magnetOutPath).Error("Failed to write magnet link file")
		} else {
			log.WithField("path", magnetOutPath).Info("Successfully generated magnet link file")
		}
	}

	return nil
}

// writeMagnetFile writes the magnet URI string to the specified file path.
func writeMagnetFile(filePath string, magnetURI string) error {
	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("error creating magnet file %s: %w", filePath, err)
	}
	defer f.Close()

	_, err = f.WriteString(magnetURI)
	if err != nil {
		return fmt.Errorf("error writing magnet file %s: %w", filePath, err)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(torrentCmd)

	torrentCmd.Flags().StringSliceVar(&announceURLs, "announce", []string{}, "Tracker announce URL (repeatable)")
	torrentCmd.Flags().IntSliceVar(&torrentModelIDs, "model-id", []int{}, "Specific model ID(s) to generate torrents for (comma-separated or repeated). Default: all downloaded models.")
	torrentCmd.Flags().StringVarP(&torrentOutputDir, "output-dir", "o", "", "Directory to save generated .torrent files (default: same directory as model file)")
	torrentCmd.Flags().BoolVarP(&overwriteTorrents, "overwrite", "f", false, "Overwrite existing .torrent files")
	torrentCmd.Flags().BoolVar(&generateMagnetLinks, "magnet-links", false, "Generate a .txt file containing the magnet link alongside each .torrent file")
	torrentCmd.Flags().IntP("concurrency", "c", 4, "Number of concurrent torrent generation workers")
}
