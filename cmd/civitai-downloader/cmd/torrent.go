package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/models"
)

var (
	torrentModelIDs   []int
	announceURLs      []string
	torrentOutputDir  string
	overwriteTorrents bool
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

		log.Infof("Generating torrents for %d model files...", len(targetDownloads))

		successCount := 0
		failCount := 0

		for _, dl := range targetDownloads {
			if dl.Folder == "" || dl.Filename == "" {
				log.WithFields(log.Fields{
					"modelID":   dl.Version.ModelId,
					"versionID": dl.Version.ID,
				}).Warn("Skipping torrent generation: Folder or Filename missing.")
				failCount++
				continue
			}
			// Original file path (still useful for logging the specific model file context)
			modelFilePath := filepath.Join(globalConfig.SavePath, dl.Folder, dl.Filename)
			// Directory path - this is what we want to torrent
			dirPath := filepath.Join(globalConfig.SavePath, dl.Folder)

			log.WithFields(log.Fields{
				"modelID":   dl.Version.ModelId,
				"versionID": dl.Version.ID,
				"directory": dirPath,
				"modelFile": modelFilePath, // Keep for context
			}).Info("Processing model directory for torrent generation")

			err := generateTorrentFile(dirPath, announceURLs, torrentOutputDir, overwriteTorrents)
			if err != nil {
				log.WithError(err).Errorf("Error generating torrent for directory %s", dirPath)
				failCount++
			} else {
				successCount++
			}
		}

		log.Infof("Torrent generation complete. Success: %d, Failed: %d", successCount, failCount)
		if failCount > 0 {
			log.Errorf("%d torrents failed to generate", failCount)
			return fmt.Errorf("%d torrents failed to generate", failCount)
		}
		return nil
	},
}

// generateTorrentFile creates a .torrent file for the given sourcePath (file or directory).
func generateTorrentFile(sourcePath string, trackers []string, outputDir string, overwrite bool) error {
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

	log.Infof("Successfully generated torrent: %s", outPath)
	return nil
}

func init() {
	rootCmd.AddCommand(torrentCmd)

	torrentCmd.Flags().StringSliceVar(&announceURLs, "announce", []string{}, "Tracker announce URL (repeatable)")
	torrentCmd.Flags().IntSliceVar(&torrentModelIDs, "model-id", []int{}, "Specific model ID(s) to generate torrents for (comma-separated or repeated). Default: all downloaded models.")
	torrentCmd.Flags().StringVarP(&torrentOutputDir, "output-dir", "o", "", "Directory to save generated .torrent files (default: same directory as model file)")
	torrentCmd.Flags().BoolVarP(&overwriteTorrents, "overwrite", "f", false, "Overwrite existing .torrent files")
}
