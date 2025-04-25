package helpers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"strings"

	"go-civitai-download/internal/models" // Import the models package

	log "github.com/sirupsen/logrus"
	"lukechampine.com/blake3"
)

// CheckHash verifies a file against provided hashes (BLAKE3, CRC32, SHA256).
// It returns true if any of the hashes match.
func CheckHash(filepath string, hashes models.Hashes) bool {
	if _, err := os.Stat(filepath); err == nil {
		file, err := os.ReadFile(filepath)
		if err != nil {
			log.WithError(err).Errorf("Error reading file %s for hash check", filepath) // Use logrus
			return false
		}

		// Check BLAKE3 hash
		if hashes.BLAKE3 != "" { // Only check if the hash is provided
			blake3Hash := blake3.Sum256(file)
			calculatedBlake3 := strings.ToUpper(hex.EncodeToString(blake3Hash[:]))
			apiBlake3 := strings.ToUpper(strings.TrimSpace(hashes.BLAKE3))
			if calculatedBlake3 == apiBlake3 {
				log.WithField("hash", "BLAKE3").Debugf("Hash match for %s", filepath)
				return true
			}
		}

		// Check CRC32 Hash
		if hashes.CRC32 != "" { // Only check if the hash is provided
			crc32Hasher := crc32.NewIEEE()
			if _, err := io.Copy(crc32Hasher, bytes.NewReader(file)); err != nil {
				log.WithError(err).Warnf("Error calculating CRC32 hash for %s", filepath) // Use logrus
			} else {
				calculatedCrc32 := fmt.Sprintf("%x", crc32Hasher.Sum32())
				apiCrc32 := strings.ToLower(strings.TrimSpace(hashes.CRC32))
				if apiCrc32 == calculatedCrc32 {
					log.WithField("hash", "CRC32").Debugf("Hash match for %s", filepath)
					return true
				}
			}
		}

		// Check SHA256 Hash
		if hashes.SHA256 != "" { // Only check if the hash is provided
			sha256Hasher := sha256.New()
			sha256Hasher.Write(file)
			calculatedSha256 := hex.EncodeToString(sha256Hasher.Sum(nil))
			apiSha256 := strings.ToLower(strings.TrimSpace(hashes.SHA256))
			if apiSha256 == calculatedSha256 {
				log.WithField("hash", "SHA256").Debugf("Hash match for %s", filepath)
				return true
			}
		}
	} else if !os.IsNotExist(err) {
		// Log error only if it's not a "file not found" error
		log.WithError(err).Warnf("Error stating file %s during hash check", filepath)
	}

	return false
}

// CounterWriter tracks the number of bytes written to the underlying writer.
// It's used to display download progress.
// Note: Consider moving this to the 'downloader' package later.
type CounterWriter struct {
	Total  uint64
	Writer io.Writer
}

// Write implements the io.Writer interface for CounterWriter.
func (cw *CounterWriter) Write(p []byte) (int, error) {
	n, err := cw.Writer.Write(p)
	cw.Total += uint64(n)
	// Progress reporting might be handled differently in CLI context
	// fmt.Printf("\rDownloaded %s", BytesToSize(cw.Total))
	return n, err
}

// BytesToSize converts a byte count into a human-readable string (KB, MB, GB, etc.).
func BytesToSize(bytes uint64) string {
	sizes := []string{"B", "KB", "MB", "GB", "TB"}
	if bytes == 0 {
		return "0B"
	}
	i := int(math.Floor(math.Log(float64(bytes)) / math.Log(1024)))
	if i >= len(sizes) {
		i = len(sizes) - 1 // Handle very large sizes
	}
	return fmt.Sprintf("%.2f%s", float64(bytes)/math.Pow(1024, float64(i)), sizes[i])
}

// ConvertToSlug converts a string into a filesystem-friendly slug.
func ConvertToSlug(str string) string {
	str = strings.ReplaceAll(str, " ", "_")
	str = strings.ReplaceAll(str, ":", "-")
	str = strings.ToLower(str)

	allowedChars := "0123456789abcdefghijklmnopqrstuvwxyz._-"

	var filteredDescription strings.Builder
	for _, ch := range str {
		if strings.ContainsRune(allowedChars, ch) {
			filteredDescription.WriteRune(ch)
		}
	}
	str = filteredDescription.String()

	// Simplify repeated separators
	for strings.Contains(str, "--") {
		str = strings.ReplaceAll(str, "--", "-")
	}
	for strings.Contains(str, "__") {
		str = strings.ReplaceAll(str, "__", "_")
	}
	str = strings.ReplaceAll(str, "-_", "-")
	str = strings.ReplaceAll(str, "_-", "-")

	// Remove leading/trailing separators
	str = strings.Trim(str, "_-")

	return str
}

// CheckAndMakeDir ensures a directory exists, creating it if necessary.
// Uses standard directory permissions (0700).
func CheckAndMakeDir(dir string) bool {
	// Use MkdirAll to create parent directories if they don't exist
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		log.WithError(err).Errorf("Error creating directory %s", dir) // Use logrus
		return false
	}
	return true
}

// TODO: Move loadConfig function to internal/config/config.go
