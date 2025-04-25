package database

import (
	"bytes" // For buffer operations
	"compress/gzip"
	"errors"
	"fmt"
	"io" // For io.ReadAll
	"os"
	"path/filepath"
	"strconv"
	"sync"

	// For DatabaseEntry

	"git.mills.io/prologic/bitcask"
	log "github.com/sirupsen/logrus" // Use logrus aliased as log
)

// ErrNotFound is returned when a key is not found in the database.
var ErrNotFound = errors.New("key not found")

// gzipMagicBytes are the first two bytes of a gzip file.
var gzipMagicBytes = []byte{0x1f, 0x8b}

// DB wraps the bitcask database instance and provides helper methods.
type DB struct {
	db           *bitcask.Bitcask
	sync.RWMutex // Embed mutex for concurrent access control
}

// Open initializes and returns a DB instance.
func Open(path string) (*DB, error) {
	// Ensure the directory exists
	dir := filepath.Dir(path)
	if dir != "." && dir != "/" { // Avoid trying to create root or current dir explicitly
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}

	dbInstance, err := bitcask.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open bitcask database at %s: %w", path, err)
	}
	log.Infof("Database opened successfully at %s", path)
	return &DB{db: dbInstance}, nil
}

// Lock acquires a write lock.
func (d *DB) Lock() {
	d.RWMutex.Lock()
}

// Unlock releases a write lock.
func (d *DB) Unlock() {
	d.RWMutex.Unlock()
}

// RLock acquires a read lock.
func (d *DB) RLock() {
	d.RWMutex.RLock()
}

// RUnlock releases a read lock.
func (d *DB) RUnlock() {
	d.RWMutex.RUnlock()
}

// Close safely closes the database connection.
func (d *DB) Close() error {
	log.Info("Closing database...")
	// Acquire write lock to ensure no operations are in progress during close
	d.Lock()
	defer d.Unlock()
	return d.db.Close()
}

// Has checks if a key exists in the database.
func (d *DB) Has(key []byte) bool {
	d.RLock()
	defer d.RUnlock()
	return d.db.Has(key)
}

// Get retrieves the value associated with a key and decompresses it if necessary.
func (d *DB) Get(key []byte) ([]byte, error) {
	d.RLock()
	value, err := d.db.Get(key)
	d.RUnlock()

	if err != nil {
		// Check if the error is KeyNotFound
		if errors.Is(err, bitcask.ErrKeyNotFound) {
			return nil, ErrNotFound // Return our specific package error
		}
		return nil, fmt.Errorf("error getting key %s: %w", string(key), err)
	}

	// Check for gzip header and decompress if necessary
	return decompressIfGzipped(value)
}

// Put compresses and stores a key-value pair in the database.
func (d *DB) Put(key []byte, value []byte) error {
	compressedValue, err := compressGzip(value, gzip.BestCompression) // Level 9
	if err != nil {
		return fmt.Errorf("error compressing value for key %s: %w", string(key), err)
	}

	// Store the compressed value
	d.Lock()
	err = d.db.Put(key, compressedValue)
	d.Unlock()
	if err != nil {
		return fmt.Errorf("error putting compressed key %s: %w", string(key), err)
	}
	return nil
}

// Delete removes a key from the database.
func (d *DB) Delete(key []byte) error {
	d.Lock()
	err := d.db.Delete(key)
	d.Unlock() // Unlock *after* potential error check
	if err != nil {
		// Wrap error, check for KeyNotFound if deletion of non-existent key is an error
		if errors.Is(err, bitcask.ErrKeyNotFound) {
			// Consider returning nil here if deleting a non-existent key is acceptable
			return ErrNotFound // Return our specific package error here too
		}
		return fmt.Errorf("error deleting key %s: %w", string(key), err)
	}
	return nil
}

// Fold iterates over all key-value pairs, decompresses the value,
// and calls the provided function.
func (d *DB) Fold(fn func(key []byte, value []byte) error) error {
	d.RLock()
	defer d.RUnlock()

	err := d.db.Fold(func(key []byte) error {
		// Need to get the value inside the Fold callback
		// Important: Keep the main read lock for the duration of Fold
		rawValue, err := d.db.Get(key) // Get raw value (no extra locking needed)
		if err != nil {
			// Log or handle error getting value during fold?
			log.WithError(err).Warnf("Fold: Error getting value for key %s", string(key))
			return nil // Decide if errors should stop the fold
		}

		// Decompress the value
		value, err := decompressIfGzipped(rawValue)
		if err != nil {
			log.WithError(err).Warnf("Fold: Error decompressing value for key %s", string(key))
			return nil // Skip this key if decompression fails
		}

		// Call the user-provided function with the decompressed value
		return fn(key, value)
	})

	return err
}

// Keys returns a channel of all keys in the database.
// Read from the channel until it is closed.
// Ensure the database is not closed while iterating.
// Note: This acquires a read lock during iteration.
func (d *DB) Keys() <-chan []byte {
	d.RLock() // Acquire read lock on the DB wrapper mutex
	// Use a goroutine to handle unlocking after the channel is fully consumed or closed
	keysChan := d.db.Keys()
	monitoredChan := make(chan []byte)

	go func() {
		defer d.RUnlock() // Ensure wrapper mutex unlock happens when this goroutine exits
		for key := range keysChan {
			monitoredChan <- key
		}
		close(monitoredChan) // Close our channel when the original closes
	}()

	return monitoredChan
}

/*
// --- State Management Helpers ---

// StoreModelInfo serializes and stores the DatabaseEntry using the file's CRC32 hash as the key.
// NOTE: This is now handled directly in download.go using version ID keys.
func (d *DB) StoreModelInfo(entry models.DatabaseEntry) error {
	dbKey := strings.ToUpper(entry.File.Hashes.CRC32)
	if dbKey == "" {
		return errors.New("cannot store model info: file CRC32 hash is empty")
	}

	dataBytes, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("error marshalling database entry for %s: %w", entry.Filename, err)
	}

	log.Debugf("Storing model info with key %s", dbKey)
	return d.Put([]byte(dbKey), dataBytes)
}

// CheckModelExists checks if a model file (identified by CRC32 hash) exists in the database.
// NOTE: Key strategy changed to version ID.
func (d *DB) CheckModelExists(crc32Hash string) bool {
	key := strings.ToUpper(crc32Hash)
	if key == "" {
		return false
	}
	return d.Has([]byte(key))
}
*/

// --- Compression Helpers ---

// decompressIfGzipped decompresses the value if it is gzipped.
func decompressIfGzipped(value []byte) ([]byte, error) {
	// Check for gzip header and decompress if present
	if bytes.HasPrefix(value, gzipMagicBytes) {
		bReader := bytes.NewReader(value)
		gReader, err := gzip.NewReader(bReader)
		if err != nil {
			log.WithError(err).Warnf("Error creating gzip reader for value, returning raw data.")
			return value, nil // Return raw data on decompression error
		}
		defer gReader.Close()

		decompressedValue, err := io.ReadAll(gReader)
		if err != nil {
			log.WithError(err).Warnf("Error decompressing value, returning raw data.")
			return value, nil // Return raw data on decompression error
		}
		return decompressedValue, nil
	}

	// If no gzip header, return the value as is
	return value, nil
}

// compressGzip compresses the value using gzip with the specified compression level.
func compressGzip(value []byte, level int) ([]byte, error) {
	var buf bytes.Buffer
	// Use BestCompression (Level 9)
	gWriter, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		// Should generally not happen with a bytes.Buffer
		return nil, fmt.Errorf("error creating gzip writer for value: %w", err)
	}
	_, err = gWriter.Write(value)
	if err != nil {
		_ = gWriter.Close() // Attempt to close writer even on error
		return nil, fmt.Errorf("error writing compressed data for value: %w", err)
	}
	err = gWriter.Close() // Close *must* be called to flush buffers
	if err != nil {
		return nil, fmt.Errorf("error closing gzip writer for value: %w", err)
	}

	compressedValue := buf.Bytes()
	return compressedValue, nil
}

// --- Specific Helpers (Can be expanded for CLI features) ---

// GetPageState retrieves the saved page number for a given query hash.
func (d *DB) GetPageState(queryHash string) (int, error) {
	key := []byte("current_page_" + queryHash)
	pageBytes, err := d.Get(key)
	if err != nil {
		if err == bitcask.ErrKeyNotFound {
			return 1, nil // Default to page 1 if not found
		}
		return 0, fmt.Errorf("error reading page state for %s: %w", queryHash, err)
	}

	page, err := strconv.Atoi(string(pageBytes))
	if err != nil {
		return 0, fmt.Errorf("error parsing saved page number '%s': %w", string(pageBytes), err)
	}
	log.WithField("queryHash", queryHash).Debugf("Retrieved page state: %d", page)
	return page, nil
}

// SetPageState saves the next page number for a given query hash.
func (d *DB) SetPageState(queryHash string, nextPage int) error {
	key := []byte("current_page_" + queryHash)
	value := []byte(strconv.Itoa(nextPage))
	err := d.Put(key, value)
	if err != nil {
		return err // Put already wraps error
	}
	log.WithField("queryHash", queryHash).Debugf("Set page state to: %d", nextPage)
	return nil
}

// DeletePageState removes the saved page number for a given query hash.
func (d *DB) DeletePageState(queryHash string) error {
	key := []byte("current_page_" + queryHash)
	err := d.Delete(key)
	if err != nil && err != bitcask.ErrKeyNotFound {
		return fmt.Errorf("error deleting page state for %s: %w", queryHash, err)
	}
	log.WithField("queryHash", queryHash).Info("Deleted page state")
	return nil // Treat KeyNotFound as success
}

// TODO: Add functions for CLI features like ListModels, GetModelInfo, etc.
