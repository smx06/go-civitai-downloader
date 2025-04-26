package api

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	sync "sync"
	time "time"

	log "github.com/sirupsen/logrus"
)

// LoggingTransport wraps an http.RoundTripper to log request and response details.
type LoggingTransport struct {
	Transport http.RoundTripper
	logFile   *os.File
	mu        sync.Mutex
	writer    *bufio.Writer
}

// NewLoggingTransport creates a new LoggingTransport.
// It opens the specified log file for appending.
func NewLoggingTransport(transport http.RoundTripper, logFilePath string) (*LoggingTransport, error) {
	f, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open API log file %s: %w", logFilePath, err)
	}

	// Use default transport if none provided
	if transport == nil {
		transport = http.DefaultTransport
	}

	return &LoggingTransport{
		Transport: transport,
		logFile:   f,
		writer:    bufio.NewWriter(f), // Use a buffered writer
	}, nil
}

// RoundTrip executes a single HTTP transaction, logging details.
func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	startTime := time.Now()

	// Log request
	reqDump, err := httputil.DumpRequestOut(req, true)
	if err != nil {
		log.WithError(err).Error("Failed to dump API request for logging")
		// Proceed with the request anyway
	} else {
		t.writeLog(fmt.Sprintf("--- Request (%s) ---\n%s\n", startTime.Format(time.RFC3339), string(reqDump)))
	}

	// Perform the actual request
	resp, err := t.Transport.RoundTrip(req)

	duration := time.Since(startTime)

	// Log response or error
	if err != nil {
		t.writeLog(fmt.Sprintf("--- Response Error (%s, Duration: %v) ---\n%s\n", time.Now().Format(time.RFC3339), duration, err.Error()))
	} else {
		// Check Content-Type to decide whether to log body
		contentType := resp.Header.Get("Content-Type")
		logBody := strings.HasPrefix(contentType, "application/json")

		if logBody {
			// Read the body for logging
			bodyBytes, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				log.WithError(readErr).Error("Failed to read response body for logging")
				// Log headers only if body read fails
				respDump, dumpErr := httputil.DumpResponse(resp, false) // Headers only
				if dumpErr != nil {
					log.WithError(dumpErr).Error("Failed to dump response headers after body read error")
					t.writeLog(fmt.Sprintf("--- Response Headers (%s, Duration: %v) ---\nStatus: %s\n(Failed to dump headers or read body)\n", time.Now().Format(time.RFC3339), duration, resp.Status))
				} else {
					t.writeLog(fmt.Sprintf("--- Response Headers (%s, Duration: %v) ---\n%s\n(Body read failed)\n", time.Now().Format(time.RFC3339), duration, string(respDump)))
				}
				// Restore body with an empty reader? Or let the original error propagate?
				// Let the caller handle the read error; resp.Body is likely closed or unusable now.
			} else {
				// IMPORTANT: Restore the body so the caller can read it.
				resp.Body.Close() // Close the original body reader
				// Use bytes.NewReader instead of bytes.NewBuffer for the replacement body
				resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

				// Log headers and the body we read
				respDumpHeader, dumpErr := httputil.DumpResponse(resp, false) // Headers only
				if dumpErr != nil {
					log.WithError(dumpErr).Error("Failed to dump response headers for logging")
					t.writeLog(fmt.Sprintf("--- Response (%s, Duration: %v) ---\nStatus: %s\n(Failed to dump headers, body logged below)\n%s\n", time.Now().Format(time.RFC3339), duration, resp.Status, string(bodyBytes)))
				} else {
					// Log headers first, then body for clarity
					t.writeLog(fmt.Sprintf("--- Response Headers (%s, Duration: %v) ---\n%s\n--- Response Body (%s) ---\n%s\n", time.Now().Format(time.RFC3339), duration, string(respDumpHeader), contentType, string(bodyBytes)))
				}
			}
		} else {
			// Log only headers for non-JSON content types
			respDump, dumpErr := httputil.DumpResponse(resp, false)
			if dumpErr != nil {
				log.WithError(dumpErr).Error("Failed to dump non-JSON response headers for logging")
				t.writeLog(fmt.Sprintf("--- Response Headers (%s, Duration: %v, Type: %s) ---\nStatus: %s\n(Failed to dump headers)\n", time.Now().Format(time.RFC3339), duration, contentType, resp.Status))
			} else {
				t.writeLog(fmt.Sprintf("--- Response Headers (%s, Duration: %v, Type: %s) ---\n%s\n(Body not logged)\n", time.Now().Format(time.RFC3339), duration, contentType, string(respDump)))
			}
		}
	}

	// Ensure logs are written
	t.writer.Flush()

	return resp, err
}

// writeLog writes a string to the buffered writer.
func (t *LoggingTransport) writeLog(logString string) {
	_, err := t.writer.WriteString(logString + "\n\n")
	if err != nil {
		// Log to stderr if writing to file fails
		fmt.Fprintf(os.Stderr, "Error writing to API log file: %v\nLog message: %s\n", err, logString)
	}
}

// Close closes the underlying log file.
func (t *LoggingTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	errFlush := t.writer.Flush() // Ensure buffer is flushed before closing
	errClose := t.logFile.Close()
	if errFlush != nil {
		return fmt.Errorf("failed to flush API log buffer: %w", errFlush)
	}
	return errClose // Return close error if flush was successful
}
