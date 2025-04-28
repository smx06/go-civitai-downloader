package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/helpers"
	"go-civitai-download/internal/models"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// --- Retry Logic Helper --- START ---

// doRequestWithRetry performs an HTTP request with exponential backoff retries.
// It retries on network errors and specific HTTP status codes (5xx, 408, 429, 504).
func doRequestWithRetry(client *http.Client, req *http.Request, maxRetries int, initialRetryDelay time.Duration, logPrefix string) (*http.Response, []byte, error) {
	var resp *http.Response
	var err error
	var bodyBytes []byte
	_ = bodyBytes // Explicitly use bodyBytes to satisfy linter (used indirectly in logging/errors)

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Calculate backoff: initial * 2^(attempt-1)
			backoff := initialRetryDelay * time.Duration(1<<(attempt-1))
			log.Infof("[%s] Retrying request for %s in %v (Attempt %d/%d)...", logPrefix, req.URL.String(), backoff, attempt+1, maxRetries+1)
			time.Sleep(backoff)
		}

		// Clone the request for the attempt, especially important if the body is consumed.
		clonedReq := req.Clone(req.Context())
		if req.Body != nil && req.GetBody != nil {
			clonedReq.Body, err = req.GetBody()
			if err != nil {
				return nil, nil, fmt.Errorf("[%s] failed to get request body for retry clone (attempt %d): %w", logPrefix, attempt+1, err)
			}
		} else if req.Body != nil {
			// This case should ideally not happen for GET requests used here.
			// If it were POST/PUT, we'd need GetBody to be set for safe retries.
			log.Warnf("[%s] Cannot guarantee safe retry for request with non-nil body without GetBody defined (URL: %s)", logPrefix, req.URL.String())
		}

		log.Debugf("[%s] Attempt %d/%d: Sending request to %s", logPrefix, attempt+1, maxRetries+1, clonedReq.URL.String())
		resp, err = client.Do(clonedReq)

		if err != nil {
			// Network-level error
			log.WithError(err).Warnf("[%s] Attempt %d/%d failed for %s: %v", logPrefix, attempt+1, maxRetries+1, clonedReq.URL.String(), err)
			if resp != nil {
				resp.Body.Close() // Ensure body is closed even if error occurred
			}
			if attempt == maxRetries {
				return nil, nil, fmt.Errorf("[%s] network error failed after %d attempts for %s: %w", logPrefix, maxRetries+1, clonedReq.URL.String(), err)
			}
			continue // Retry
		}

		// Read the body regardless of status code
		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close() // Close body immediately after reading

		if readErr != nil {
			log.WithError(readErr).Warnf("[%s] Attempt %d/%d failed to read response body for %s: %v", logPrefix, attempt+1, maxRetries+1, clonedReq.URL.String(), readErr)
			if attempt == maxRetries {
				return nil, nil, fmt.Errorf("[%s] failed to read body after %d attempts for %s: %w", logPrefix, maxRetries+1, clonedReq.URL.String(), readErr)
			}
			continue // Retry
		}

		// Check status code
		if resp.StatusCode == http.StatusOK {
			log.Debugf("[%s] Attempt %d/%d successful for %s", logPrefix, attempt+1, maxRetries+1, clonedReq.URL.String())
			return resp, bodyBytes, nil // Success!
		}

		// Non-OK status code
		bodySample := string(bodyBytes)
		if len(bodySample) > 200 { // Limit logged body size
			bodySample = bodySample[:200] + "..."
		}
		log.Warnf("[%s] Attempt %d/%d for %s failed with status %s. Body: %s", logPrefix, attempt+1, maxRetries+1, clonedReq.URL.String(), resp.Status, bodySample)

		// Check if status code is retryable
		isRetryableStatus := resp.StatusCode >= 500 || // Server errors
			resp.StatusCode == http.StatusRequestTimeout || // 408
			resp.StatusCode == http.StatusTooManyRequests || // 429
			resp.StatusCode == http.StatusGatewayTimeout // 504

		if isRetryableStatus && attempt < maxRetries {
			log.Warnf("[%s] Status %s is retryable.", logPrefix, resp.Status)
			// Continue loop, backoff delay is handled at the start of the next iteration
		} else {
			// Not a retryable status code OR max retries reached
			errMsg := fmt.Sprintf("[%s] request failed with status %s after %d attempts", logPrefix, resp.Status, attempt+1)
			if !isRetryableStatus {
				errMsg = fmt.Sprintf("[%s] request failed with non-retryable status %s on attempt %d", logPrefix, resp.Status, attempt+1)
			}
			// Include body sample in final error if it's not success
			errMsg += fmt.Sprintf(". Body: %s", bodySample)
			return resp, bodyBytes, fmt.Errorf(errMsg)
		}
	} // End of retry loop

	// Should not be reachable
	return nil, nil, fmt.Errorf("[%s] retry loop completed without success or error return for %s", logPrefix, req.URL.String())
}

// --- Retry Logic Helper --- END ---

// passesFileFilters checks if a given file passes the configured file-level filters.
func passesFileFilters(file models.File, modelType string) bool {
	// Check hash presence (essential)
	if file.Hashes.CRC32 == "" {
		log.Debugf("Skipping file %s: Missing CRC32 hash.", file.Name)
		return false
	}

	// Check primary file filter
	if viper.GetBool("primaryonly") && !file.Primary {
		log.Debugf("Skipping non-primary file %s.", file.Name)
		return false
	}

	// Check format (basic check)
	if file.Metadata.Format == "" {
		log.Debugf("Skipping file %s: Missing metadata format.", file.Name)
		return false
	}
	// TODO: Make acceptable formats configurable?
	if strings.ToLower(file.Metadata.Format) != "safetensor" {
		log.Debugf("Skipping non-safetensor file %s (Format: %s).", file.Name, file.Metadata.Format)
		return false
	}

	// Check checkpoint-specific filters (pruned, fp16)
	if strings.EqualFold(modelType, "checkpoint") {
		sizeStr := fmt.Sprintf("%v", file.Metadata.Size)
		fpStr := fmt.Sprintf("%v", file.Metadata.Fp)

		if viper.GetBool("pruned") && !strings.EqualFold(sizeStr, "pruned") {
			log.Debugf("Skipping non-pruned file %s (Size: %s) in checkpoint model.", file.Name, sizeStr)
			return false
		}
		if viper.GetBool("fp16") && !strings.EqualFold(fpStr, "fp16") {
			log.Debugf("Skipping non-fp16 file %s (FP: %s) in checkpoint model.", file.Name, fpStr)
			return false
		}
	}

	// --- Filter by ignored filename substrings --- (Case-Insensitive)
	ignoredFilenameStrings := viper.GetStringSlice("ignorefilenamestrings") // Use Viper
	if len(ignoredFilenameStrings) > 0 {
		for _, ignoreFileName := range ignoredFilenameStrings {
			if ignoreFileName != "" && strings.Contains(strings.ToLower(file.Name), strings.ToLower(ignoreFileName)) {
				log.Debugf("      - Skipping file %s: Filename contains ignored string '%s'.", file.Name, ignoreFileName)
				return false
			}
		}
	}

	// If all checks passed
	return true
}

// handleSingleVersionDownload Fetches details for a specific model version ID and processes it for download.
func handleSingleVersionDownload(versionID int, db *database.DB, client *http.Client, cfg *models.Config, _ *cobra.Command) ([]potentialDownload, uint64, error) {
	log.Debugf("Fetching details for model version ID: %d", versionID)
	apiURL := fmt.Sprintf("https://civitai.com/api/v1/model-versions/%d", versionID)
	logPrefix := fmt.Sprintf("Version %d", versionID) // For retry logging

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request for version %d: %w", versionID, err)
	}
	if cfg.ApiKey != "" {
		req.Header.Add("Authorization", "Bearer "+cfg.ApiKey)
	}

	// --- Use Retry Helper ---
	maxRetries := viper.GetInt("maxretries")
	initialRetryDelay := time.Duration(viper.GetInt("initialretrydelayms")) * time.Millisecond
	// Assign the unused resp to the blank identifier `_`
	_, bodyBytes, err := doRequestWithRetry(client, req, maxRetries, initialRetryDelay, logPrefix)
	// --- End Use Retry Helper ---

	if err != nil {
		// Error already includes context from doRequestWithRetry (status, attempts)
		// We might add a bit more context here if needed.
		// If resp is not nil, the error message likely contains the status code.
		// If resp is nil, it was likely a network error or read error after all retries.
		finalErrMsg := fmt.Sprintf("failed to fetch version %d: %v", versionID, err)
		// Check if the error message already contains the body snippet
		if !strings.Contains(err.Error(), "Body:") && len(bodyBytes) > 0 {
			bodySample := string(bodyBytes)
			if len(bodySample) > 200 {
				bodySample = bodySample[:200] + "..."
			}
			finalErrMsg += fmt.Sprintf(". Last Body: %s", bodySample)
		}
		return nil, 0, fmt.Errorf(finalErrMsg)
	}
	// If err is nil, we know resp is not nil and resp.StatusCode was http.StatusOK
	// bodyBytes contains the successful response body.

	var versionResponse models.ModelVersion // Use the updated struct from models.go
	if err := json.Unmarshal(bodyBytes, &versionResponse); err != nil {
		log.WithError(err).Errorf("Response body sample: %s", string(bodyBytes[:min(len(bodyBytes), 200)]))
		return nil, 0, fmt.Errorf("failed to decode API response for version %d: %w", versionID, err)
	}

	log.Infof("Successfully fetched details for version %d (%s) of model %s (%s)",
		versionResponse.ID, versionResponse.Name, versionResponse.Model.Name, versionResponse.Model.Type)

	// --- Convert to potentialDownload ---
	var potentialDownloadsPage []potentialDownload
	versionWithoutFilesImages := versionResponse // Create a copy for metadata
	versionWithoutFilesImages.Files = nil
	versionWithoutFilesImages.Images = nil

	// Use a placeholder creator if not directly available in the response
	placeholderCreator := models.Creator{Username: "unknown_creator"}

	for _, file := range versionResponse.Files {
		// Use the new shared filtering function
		if !passesFileFilters(file, versionResponse.Model.Type) {
			continue // Skip this file if it doesn't pass filters
		}

		// --- Path/Filename Construction (Copied/adapted from pagination loop) ---
		var slug string
		modelTypeName := helpers.ConvertToSlug(versionResponse.Model.Type)
		baseModelStr := versionResponse.BaseModel
		if baseModelStr == "" {
			baseModelStr = "unknown-base"
		}
		baseModelSlug := helpers.ConvertToSlug(baseModelStr)
		modelNameSlug := helpers.ConvertToSlug(versionResponse.Model.Name)
		// --- Modify slug construction for type/base/model structure ---
		slug = filepath.Join(modelTypeName, modelNameSlug, baseModelSlug)
		// --- End slug construction modification ---

		// --- Create version specific slug based on file name (without extension) ---
		fileNameWithoutExt := strings.TrimSuffix(file.Name, filepath.Ext(file.Name))
		versionSlug := fmt.Sprintf("%d-%s", versionResponse.ID, helpers.ConvertToSlug(fileNameWithoutExt))
		// --- End version specific slug ---

		baseFileName := helpers.ConvertToSlug(file.Name)
		ext := filepath.Ext(baseFileName)
		baseFileName = strings.TrimSuffix(baseFileName, ext)
		if strings.ToLower(file.Metadata.Format) == "safetensor" && !strings.EqualFold(ext, ".safetensors") {
			ext = ".safetensors"
		}
		if ext == "" {
			ext = ".bin"
			log.Warnf("File %s in model version %d has no extension, defaulting to '.bin'", file.Name, versionID)
		}
		finalBaseFilenameOnly := baseFileName + ext
		dbKeySimple := strings.ToUpper(file.Hashes.CRC32)
		metaSuffixParts := []string{dbKeySimple}
		if strings.EqualFold(versionResponse.Model.Type, "checkpoint") {
			if fpStr := fmt.Sprintf("%v", file.Metadata.Fp); fpStr != "" {
				metaSuffixParts = append(metaSuffixParts, helpers.ConvertToSlug(fpStr))
			}
			if sizeStr := fmt.Sprintf("%v", file.Metadata.Size); sizeStr != "" {
				metaSuffixParts = append(metaSuffixParts, helpers.ConvertToSlug(sizeStr))
			}
		}
		metaSuffix := "-" + strings.Join(metaSuffixParts, "-")
		constructedFileNameWithSuffix := baseFileName + metaSuffix + ext
		// --- Modify directory path to include version ---
		fullDirPath := filepath.Join(cfg.SavePath, slug, versionSlug)
		// --- End directory path modification ---
		fullFilePath := filepath.Join(fullDirPath, constructedFileNameWithSuffix)
		// --- End Path/Filename Construction ---

		pd := potentialDownload{
			ModelName:         versionResponse.Model.Name,
			ModelType:         versionResponse.Model.Type,
			VersionName:       versionResponse.Name,
			BaseModel:         versionResponse.BaseModel,
			Creator:           placeholderCreator,
			File:              file,
			ModelVersionID:    versionResponse.ID,
			TargetFilepath:    fullFilePath,
			Slug:              slug,
			FinalBaseFilename: finalBaseFilenameOnly,
			CleanedVersion:    versionWithoutFilesImages,
			FullVersion:       versionResponse,
			OriginalImages:    versionResponse.Images,
		}
		potentialDownloadsPage = append(potentialDownloadsPage, pd)
		log.Debugf("Passed filters for single version: %s -> %s", file.Name, fullFilePath)

	} // End file loop for this version

	if len(potentialDownloadsPage) == 0 {
		log.Infof("No files passed filters for model version %d.", versionID)
		return nil, 0, nil // No error, just no files to download
	}

	// --- Process against DB (Uses processPage moved to cmd_download_processing.go) ---
	log.Debugf("Checking %d potential downloads from version %d against database...", len(potentialDownloadsPage), versionID)
	// Assuming processPage is available in this package after refactoring
	queuedFromPage, sizeFromPage := processPage(db, potentialDownloadsPage, cfg)
	if len(queuedFromPage) > 0 {
		log.Infof("Queued %d file(s) (Size: %s) from version %d after DB check.", len(queuedFromPage), helpers.BytesToSize(sizeFromPage), versionID)
	} else {
		log.Debugf("No new files queued from version %d after DB check.", versionID)
	}

	return queuedFromPage, sizeFromPage, nil
}

// handleSingleModelDownload Fetches details for a specific model ID and processes its versions/files for download.
// It now also accepts imageDownloader to handle --model-images.
func handleSingleModelDownload(modelID int, db *database.DB, client *http.Client, imageDownloader *downloader.Downloader, cfg *models.Config, cmd *cobra.Command) ([]potentialDownload, uint64, error) {
	log.Debugf("Fetching details for model ID: %d", modelID)
	apiURL := fmt.Sprintf("https://civitai.com/api/v1/models/%d", modelID)
	logPrefix := fmt.Sprintf("Model %d", modelID) // For retry logging

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request for model %d: %w", modelID, err)
	}
	if cfg.ApiKey != "" {
		req.Header.Add("Authorization", "Bearer "+cfg.ApiKey)
	}

	// --- Use Retry Helper ---
	maxRetries := viper.GetInt("maxretries")
	initialRetryDelay := time.Duration(viper.GetInt("initialretrydelayms")) * time.Millisecond
	// Assign the unused resp to the blank identifier `_`
	_, bodyBytes, err := doRequestWithRetry(client, req, maxRetries, initialRetryDelay, logPrefix)
	// --- End Use Retry Helper ---

	if err != nil {
		// Error already includes context from doRequestWithRetry
		finalErrMsg := fmt.Sprintf("failed to fetch model %d: %v", modelID, err)
		if !strings.Contains(err.Error(), "Body:") && len(bodyBytes) > 0 {
			bodySample := string(bodyBytes)
			if len(bodySample) > 200 {
				bodySample = bodySample[:200] + "..."
			}
			finalErrMsg += fmt.Sprintf(". Last Body: %s", bodySample)
		}
		return nil, 0, fmt.Errorf(finalErrMsg)
	}
	// Success case: resp.StatusCode == http.StatusOK and bodyBytes is valid

	var modelResponse models.Model // Use the full Model struct
	if err := json.Unmarshal(bodyBytes, &modelResponse); err != nil {
		log.WithError(err).Errorf("Response body sample: %s", string(bodyBytes[:min(len(bodyBytes), 200)]))
		return nil, 0, fmt.Errorf("failed to decode API response for model %d: %w", modelID, err)
	}

	log.Infof("Successfully fetched details for model %d (%s) - Type: %s",
		modelResponse.ID, modelResponse.Name, modelResponse.Type)

	// --- Handle --model-info and --model-images --- (New Section)
	saveFullInfo := viper.GetBool("savemodelinfo") // Viper key from download.go init
	if saveFullInfo {
		// Determine baseModelSlug based on the latest version for consistent pathing
		latestInfoVersion := models.ModelVersion{}
		latestInfoTime := time.Time{}
		if len(modelResponse.ModelVersions) > 0 {
			for _, v := range modelResponse.ModelVersions {
				pAt, errP := time.Parse(time.RFC3339Nano, v.PublishedAt)
				if errP != nil {
					pAt, _ = time.Parse(time.RFC3339, v.PublishedAt)
				}
				if errP == nil && (latestInfoVersion.ID == 0 || pAt.After(latestInfoTime)) {
					latestInfoTime = pAt
					latestInfoVersion = v
				}
			}
		}
		baseModelSlug := "unknown_base_model"
		if latestInfoVersion.ID != 0 {
			baseModelSlug = helpers.ConvertToSlug(latestInfoVersion.BaseModel)
			if baseModelSlug == "" {
				baseModelSlug = "unknown_base_model"
			}
		}

		// --- Modify slug construction for type/model structure (removing base model for info/images) ---
		modelInfoSlug := filepath.Join(helpers.ConvertToSlug(modelResponse.Type), helpers.ConvertToSlug(modelResponse.Name))
		// --- End slug construction modification ---
		modelBaseDir := filepath.Join(cfg.SavePath, modelInfoSlug) // Path for model info/images

		// Pass the new modelBaseDir to saveModelInfoFile
		if err := saveModelInfoFile(modelResponse, modelBaseDir); err != nil {
			log.WithError(err).Warnf("Failed to save full model info for model %d (%s)", modelResponse.ID, modelResponse.Name)
			// Don't stop processing just because info saving failed
		}

		saveModelImages := viper.GetBool("savemodelimages") // Viper key from download.go init
		if saveModelImages {
			if imageDownloader == nil {
				log.Warnf("Skipping --model-images download for model %d: Image downloader not initialized.", modelID)
			} else {
				logPrefix := fmt.Sprintf("Model %d Img", modelID)
				log.Infof("[%s] Processing all model images for %s (%d)...", logPrefix, modelResponse.Name, modelID)
				// --- Adjust image paths to new structure ---
				modelImagesBaseDir := filepath.Join(modelBaseDir, "images")
				// --- End image path adjustment ---
				var totalImgSuccess, totalImgFail int = 0, 0

				// Get concurrency level from command flags
				concurrency, _ := cmd.Flags().GetInt("concurrency")
				if concurrency <= 0 {
					concurrency = 4
				} // Default concurrency

				for _, version := range modelResponse.ModelVersions {
					versionLogPrefix := fmt.Sprintf("%s v%d", logPrefix, version.ID)
					// --- Adjust version image path ---
					versionImagesDir := filepath.Join(modelImagesBaseDir, fmt.Sprintf("%d", version.ID))
					// --- End version image path adjustment ---
					log.Debugf("[%s] Checking %d images for version %s (%d)", versionLogPrefix, len(version.Images), version.Name, version.ID)
					if len(version.Images) > 0 {
						log.Debugf("[%s] Calling downloadImages for %d images...", versionLogPrefix, len(version.Images))
						// Use the existing downloadImages helper
						imgSuccess, imgFail := downloadImages(versionLogPrefix, version.Images, versionImagesDir, imageDownloader, concurrency)
						totalImgSuccess += imgSuccess
						totalImgFail += imgFail
					}
				}
				log.Infof("[%s] Finished processing images for model %s (%d). Total Success: %d, Total Failed: %d",
					logPrefix, modelResponse.Name, modelID, totalImgSuccess, totalImgFail)
			}
		}
	} // --- End Handle --model-info and --model-images ---

	// --- Process versions and files from the model response ---
	var potentialDownloadsFromModel []potentialDownload
	versionsToProcess := []models.ModelVersion{}

	// Use Viper to get all-versions flag
	downloadAll := viper.GetBool("downloadallversions") // Viper key from download.go init
	if downloadAll {
		log.Debugf("Processing all %d versions for model %s (%d) due to --all-versions flag.", len(modelResponse.ModelVersions), modelResponse.Name, modelID)
		if len(modelResponse.ModelVersions) == 0 {
			log.Warnf("Model %s (%d) has no versions listed to process.", modelResponse.Name, modelID)
			return nil, 0, nil // No versions, no error
		}
		versionsToProcess = modelResponse.ModelVersions
	} else {
		// Find the latest version if not downloading all
		latestVersion := models.ModelVersion{}
		latestTime := time.Time{}
		if len(modelResponse.ModelVersions) == 0 {
			log.Warnf("Model %s (%d) has no versions listed to process.", modelResponse.Name, modelID)
			return nil, 0, nil // No versions, no error
		}
		for _, version := range modelResponse.ModelVersions {
			if version.PublishedAt == "" {
				log.Warnf("Skipping version %s in model %s (%d): PublishedAt timestamp is empty.", version.Name, modelResponse.Name, modelID)
				continue
			}
			publishedAt, errParse := time.Parse(time.RFC3339Nano, version.PublishedAt)
			if errParse != nil {
				publishedAt, errParse = time.Parse(time.RFC3339, version.PublishedAt)
				if errParse != nil {
					log.WithError(errParse).Warnf("Skipping version %s in model %s (%d): Error parsing time '%s'", version.Name, modelResponse.Name, modelID, version.PublishedAt)
					continue
				}
			}
			if latestVersion.ID == 0 || publishedAt.After(latestTime) {
				latestTime = publishedAt
				latestVersion = version
			}
		}
		if latestVersion.ID == 0 {
			log.Warnf("No valid latest version found for model %s (%d). Skipping.", modelResponse.Name, modelID)
			return nil, 0, nil // No valid latest version
		}
		log.Debugf("Processing latest version %s (%d) for model %s (%d).", latestVersion.Name, latestVersion.ID, modelResponse.Name, modelID)
		versionsToProcess = append(versionsToProcess, latestVersion)
	}

	// --- Loop through selected versions and process files ---
	for _, currentVersion := range versionsToProcess {
		log.Debugf("Processing files for version %s (%d) of model %s (%d)", currentVersion.Name, currentVersion.ID, modelResponse.Name, modelID)
		// --- Filter by ignored base models --- (Case-Insensitive)
		ignoredBaseModels := viper.GetStringSlice("ignorebasemodels") // Use Viper
		if len(ignoredBaseModels) > 0 {
			versionBaseModelLower := strings.ToLower(currentVersion.BaseModel)
			for _, ignoreBaseModel := range ignoredBaseModels {
				if ignoreBaseModel != "" && strings.Contains(versionBaseModelLower, strings.ToLower(ignoreBaseModel)) {
					log.Debugf("    - Skipping version %s: Base model '%s' contains ignored string '%s'.", currentVersion.Name, currentVersion.BaseModel, ignoreBaseModel)
					continue // Skip to next version
				}
			}
		}

		// Prepare cleaned version for metadata/DB
		versionWithoutFilesImages := currentVersion
		versionWithoutFilesImages.Files = nil
		versionWithoutFilesImages.Images = nil

	fileLoop: // Label for continue
		for _, file := range currentVersion.Files { // Use files from currentVersion
			// Use the shared filtering function
			if !passesFileFilters(file, modelResponse.Type) {
				continue fileLoop // Skip this file if it doesn't pass filters
			}

			// --- Path/Filename Construction (using currentVersion) ---
			var slug string // Now only used for file path
			modelTypeName := helpers.ConvertToSlug(modelResponse.Type)
			baseModelStr := currentVersion.BaseModel // Use currentVersion
			if baseModelStr == "" {
				baseModelStr = "unknown-base"
			}
			baseModelSlug := helpers.ConvertToSlug(baseModelStr)
			modelNameSlug := helpers.ConvertToSlug(modelResponse.Name)
			// --- Modify slug construction for type/model/base structure ---
			slug = filepath.Join(modelTypeName, modelNameSlug, baseModelSlug) // Changed order for file path
			// --- End slug construction modification ---

			// --- Create version specific slug based on file name (without extension) ---
			fileNameWithoutExt := strings.TrimSuffix(file.Name, filepath.Ext(file.Name))
			versionSlug := fmt.Sprintf("%d-%s", currentVersion.ID, helpers.ConvertToSlug(fileNameWithoutExt))
			// --- End version specific slug ---

			baseFileName := helpers.ConvertToSlug(file.Name)
			ext := filepath.Ext(baseFileName)
			baseFileName = strings.TrimSuffix(baseFileName, ext)
			if strings.ToLower(file.Metadata.Format) == "safetensor" && !strings.EqualFold(ext, ".safetensors") {
				ext = ".safetensors"
			}
			if ext == "" {
				ext = ".bin"
				log.Warnf("File %s in version %s (%d) has no extension, defaulting to '.bin'", file.Name, currentVersion.Name, currentVersion.ID)
			}
			finalBaseFilenameOnly := baseFileName + ext
			constructedFileNameOnly := baseFileName + ext // Just base + extension
			// --- Modify directory path to include version ---
			fullDirPath := filepath.Join(cfg.SavePath, slug, versionSlug)
			// --- End directory path modification ---
			fullFilePath := filepath.Join(fullDirPath, constructedFileNameOnly) // Use filename without suffix
			// --- End Path/Filename Construction ---

			// Create potentialDownload using currentVersion data
			pd := potentialDownload{
				ModelName:         modelResponse.Name,
				ModelType:         modelResponse.Type,
				VersionName:       currentVersion.Name,      // Use currentVersion
				BaseModel:         currentVersion.BaseModel, // Use currentVersion
				Creator:           modelResponse.Creator,
				File:              file,
				ModelVersionID:    currentVersion.ID, // Use currentVersion
				TargetFilepath:    fullFilePath,      // Path without suffix
				Slug:              slug,
				FinalBaseFilename: finalBaseFilenameOnly,     // Keep original base+ext for reference
				CleanedVersion:    versionWithoutFilesImages, // Use cleaned currentVersion
				FullVersion:       currentVersion,            // Store the full original version data
				OriginalImages:    currentVersion.Images,     // Use currentVersion images
			}
			potentialDownloadsFromModel = append(potentialDownloadsFromModel, pd)
			// Log the intended path *without* suffix for clarity in this phase
			log.Debugf("Passed filters: %s (Model: %s (%d), Version: %s (%d)) -> %s", file.Name, modelResponse.Name, modelID, currentVersion.Name, currentVersion.ID, fullFilePath)
		} // End fileLoop
	} // --- End version loop ---

	if len(potentialDownloadsFromModel) == 0 {
		log.Infof("No files passed filters for model ID %d.", modelID)
		return nil, 0, nil // No error, just no files to download
	}

	// --- Process against DB (Uses processPage) ---
	log.Debugf("Checking %d potential downloads from model %d against database...", len(potentialDownloadsFromModel), modelID)
	queuedFromModel, sizeFromModel := processPage(db, potentialDownloadsFromModel, cfg)
	if len(queuedFromModel) > 0 {
		log.Infof("Queued %d file(s) (Size: %s) from model %d after DB check.", len(queuedFromModel), helpers.BytesToSize(sizeFromModel), modelID)
	} else {
		log.Debugf("No new files queued from model %d after DB check.", modelID)
	}

	return queuedFromModel, sizeFromModel, nil
}

// fetchModelsPaginated handles the process of fetching models using API pagination.
func fetchModelsPaginated(db *database.DB, client *http.Client, imageDownloader *downloader.Downloader, queryParams models.QueryParameters, cfg *models.Config, cmd *cobra.Command) ([]potentialDownload, uint64, error) {
	var allPotentialDownloads []potentialDownload
	var totalQueuedSizeBytes uint64
	pageCount := 0
	nextCursor := ""         // Start with no cursor
	totalModelsReceived := 0 // Counter for total models *received* across pages for limit check

	// Get max pages and retry config from Viper
	maxPages := viper.GetInt("maxpages")    // Viper key from download.go init
	userTotalLimit := viper.GetInt("limit") // User's intended total limit (0 = unlimited)
	maxRetries := viper.GetInt("maxretries")
	initialRetryDelay := time.Duration(viper.GetInt("initialretrydelayms")) * time.Millisecond
	apiDelayMs := viper.GetInt("apidelayms") // Viper key from root.go init

	for {
		pageCount++
		if maxPages > 0 && pageCount > maxPages {
			log.Infof("Reached max page limit (%d). Stopping pagination.", maxPages)
			break
		}

		// Construct API URL with query parameters
		apiURL := "https://civitai.com/api/v1/models"
		params := url.Values{}
		// Use API default/max limit per page (e.g., 100) for efficiency.
		// Do NOT send the user's total limit here.
		params.Set("limit", "100") // Request max items per page

		// Only set query param if it's not empty
		if queryParams.Query != "" {
			params.Set("query", queryParams.Query)
		}
		if queryParams.Tag != "" {
			params.Set("tag", queryParams.Tag)
		}
		if queryParams.Username != "" {
			params.Set("username", queryParams.Username)
		}
		if len(queryParams.Types) > 0 {
			params.Set("types", strings.Join(queryParams.Types, ","))
		}
		if queryParams.Sort != "" {
			params.Set("sort", queryParams.Sort)
		}
		if queryParams.Period != "" {
			params.Set("period", queryParams.Period)
		}
		if queryParams.Rating > 0 {
			params.Set("rating", fmt.Sprintf("%d", queryParams.Rating))
		}
		if queryParams.Favorites {
			params.Set("favorites", "true")
		}
		if queryParams.Hidden {
			params.Set("hidden", "true")
		}
		if queryParams.PrimaryFileOnly {
			params.Set("primaryFileOnly", "true")
		}
		if !queryParams.AllowNoCredit {
			params.Set("allowNoCredit", "false")
		}
		if !queryParams.AllowDerivatives {
			params.Set("allowDerivatives", "false")
		}
		if !queryParams.AllowDifferentLicenses {
			params.Set("allowDifferentLicenses", "false")
		}
		if queryParams.AllowCommercialUse != "Any" {
			params.Set("allowCommercialUse", queryParams.AllowCommercialUse)
		}
		// Always set nsfw parameter to true or false
		if queryParams.Nsfw {
			params.Set("nsfw", "true")
		} else {
			params.Set("nsfw", "false")
		}
		if len(queryParams.BaseModels) > 0 {
			params.Set("baseModels", strings.Join(queryParams.BaseModels, ","))
		}

		if nextCursor != "" {
			params.Set("cursor", nextCursor)
			log.Infof("Requesting next page %d with cursor: %s...", pageCount, nextCursor)
		} else {
			log.Infof("Requesting API page %d...", pageCount)
		}

		fullURL := fmt.Sprintf("%s?%s", apiURL, params.Encode())
		log.Debugf("API Request URL: %s", fullURL)
		logPrefix := fmt.Sprintf("Page %d", pageCount) // For retry logging

		// --- Check for debug flag --- NEW
		if printUrl, _ := cmd.Flags().GetBool("debug-print-api-url"); printUrl {
			fmt.Println(fullURL) // Print only the URL to stdout
			os.Exit(0)           // Exit immediately
		}
		// --- End check for debug flag --- NEW

		req, err := http.NewRequest("GET", fullURL, nil)
		if err != nil {
			// This error is unlikely recoverable by retry, return directly.
			return allPotentialDownloads, totalQueuedSizeBytes, fmt.Errorf("failed to create request for page %d: %w", pageCount, err)
		}
		if cfg.ApiKey != "" { // Still need ApiKey from config
			req.Header.Add("Authorization", "Bearer "+cfg.ApiKey)
		}

		// --- Use Retry Helper ---
		// Assign the unused resp to the blank identifier `_`
		_, bodyBytes, err := doRequestWithRetry(client, req, maxRetries, initialRetryDelay, logPrefix)
		// --- End Use Retry Helper ---

		if err != nil {
			// Error already includes context from doRequestWithRetry
			// If resp is nil, it's likely a network/read error after retries.
			// If resp is not nil, it's a non-200 status after retries.
			// The error message from the helper should be descriptive enough.
			finalErrMsg := fmt.Sprintf("failed to fetch page %d: %v", pageCount, err)
			// Check if the error message already contains the body snippet
			if !strings.Contains(err.Error(), "Body:") && len(bodyBytes) > 0 {
				bodySample := string(bodyBytes)
				if len(bodySample) > 200 {
					bodySample = bodySample[:200] + "..."
				}
				finalErrMsg += fmt.Sprintf(". Last Body: %s", bodySample)
			}
			// Stop pagination on persistent error for a page
			return allPotentialDownloads, totalQueuedSizeBytes, fmt.Errorf(finalErrMsg)
		}
		// Success case: resp.StatusCode == http.StatusOK and bodyBytes is valid

		var response models.ApiResponse // Use the correct struct name
		if err := json.Unmarshal(bodyBytes, &response); err != nil {
			// Use the bodyBytes we already have for context
			bodySample := string(bodyBytes)
			if len(bodySample) > 500 { // Allow slightly more for JSON errors
				bodySample = bodySample[:500] + "..."
			}
			return allPotentialDownloads, totalQueuedSizeBytes, fmt.Errorf("failed to decode API response for page %d: %w. Body: %s", pageCount, err, bodySample)
		}

		if len(response.Items) == 0 {
			log.Info("Received empty item list from API, assuming end of results.")
			break
		}

		// --- Add to total received models count --- START ---
		// This counts models *received* from the API page for the limit check
		totalModelsReceived += len(response.Items)
		log.Debugf("Received %d models so far across all pages.", totalModelsReceived)
		// --- Add to total received models count --- END ---

		// Process metadata for cursor and total items
		if response.Metadata.NextCursor != "" {
			nextCursor = response.Metadata.NextCursor
			log.Debugf("API Metadata: TotalItems=%d, CurrentPage=%d, PageSize=%d, NextCursor=%s",
				response.Metadata.TotalItems, response.Metadata.CurrentPage, response.Metadata.PageSize, response.Metadata.NextCursor)
		} else {
			log.Info("No next cursor found. Finished fetching.") // Changed log level
			nextCursor = ""                                      // Stop loop
		}

		// --- Process Models from this Page ---
		var potentialDownloadsThisPage []potentialDownload
		log.Debugf("Processing %d models from request %d for potential downloads...", len(response.Items), pageCount)

		for _, model := range response.Items {
			// --- Save Full Model Info / Images if Flag is Set ---
			// This logic runs regardless of which versions are downloaded later
			// Use Viper to get these boolean flags
			saveFullInfo := viper.GetBool("savemodelinfo") // Viper key from download.go init
			if saveFullInfo {
				// Determine baseModelSlug based on the latest version for consistent pathing
				latestInfoVersion := models.ModelVersion{}
				latestInfoTime := time.Time{}
				if len(model.ModelVersions) > 0 {
					for _, v := range model.ModelVersions {
						pAt, errP := time.Parse(time.RFC3339Nano, v.PublishedAt)
						if errP != nil {
							pAt, _ = time.Parse(time.RFC3339, v.PublishedAt)
						}
						if errP == nil && (latestInfoVersion.ID == 0 || pAt.After(latestInfoTime)) {
							latestInfoTime = pAt
							latestInfoVersion = v
						}
					}
				}

				modelNameSlug := helpers.ConvertToSlug(model.Name)
				if modelNameSlug == "" {
					modelNameSlug = "unknown_model"
				}
				baseModelSlug := "unknown_base_model"
				if latestInfoVersion.ID != 0 {
					baseModelSlug = helpers.ConvertToSlug(latestInfoVersion.BaseModel)
					if baseModelSlug == "" {
						baseModelSlug = "unknown_base_model"
					}
				}

				// --- Modify slug construction for type/model structure (removing base model for info/images) ---
				modelInfoSlug := filepath.Join(helpers.ConvertToSlug(model.Type), modelNameSlug)
				// --- End slug construction modification ---
				modelBaseDir := filepath.Join(cfg.SavePath, modelInfoSlug) // Path for model info/images

				// Pass the new modelBaseDir to saveModelInfoFile
				if err := saveModelInfoFile(model, modelBaseDir); err != nil {
					log.WithError(err).Warnf("Failed to save full model info for model %d (%s)", model.ID, model.Name)
				}

				saveModelImages := viper.GetBool("savemodelimages") // Viper key from download.go init
				if saveModelImages {
					if !saveFullInfo {
						log.Error("--model-images requires --model-info to be set as well. Aborting image download.")
					} else {
						logPrefix := fmt.Sprintf("Model %d Img", model.ID)
						log.Infof("[%s] Processing all model images for %s (%d)...", logPrefix, model.Name, model.ID)
						// --- Adjust image paths to new structure ---
						modelImagesBaseDir := filepath.Join(modelBaseDir, "images")
						// --- End image path adjustment ---
						var totalImgSuccess, totalImgFail int = 0, 0

						// Get concurrency level from Viper
						concurrency := viper.GetInt("concurrency") // Viper key from download.go init
						if concurrency <= 0 {
							concurrency = 4
						} // Simple default if flag missing/invalid

						for _, version := range model.ModelVersions {
							versionLogPrefix := fmt.Sprintf("%s v%d", logPrefix, version.ID)
							// --- Adjust version image path ---
							versionImagesDir := filepath.Join(modelImagesBaseDir, fmt.Sprintf("%d", version.ID))
							// --- End version image path adjustment ---
							log.Debugf("[%s] Checking %d images for version %s (%d)", versionLogPrefix, len(version.Images), version.Name, version.ID)
							if len(version.Images) > 0 {
								log.Debugf("[%s] Calling downloadImages for %d images...", versionLogPrefix, len(version.Images))
								// Correct line using := and retrieved concurrency, remove nil writer argument
								imgSuccess, imgFail := downloadImages(versionLogPrefix, version.Images, versionImagesDir, imageDownloader, concurrency)
								totalImgSuccess += imgSuccess
								totalImgFail += imgFail
							}
						}
						log.Infof("[%s] Finished processing images for model %s (%d). Total Success: %d, Total Failed: %d",
							logPrefix, model.Name, model.ID, totalImgSuccess, totalImgFail)
					}
				}
			} // --- End Save Full Model Info / Images ---

			// --- Version Selection / Processing ---
			// Get value using Viper
			downloadAll := viper.GetBool("downloadallversions") // Viper key from download.go init
			versionsToProcess := []models.ModelVersion{}

			if downloadAll {
				log.Debugf("Processing all versions for model %s (%d) due to --all-versions flag.", model.Name, model.ID)
				if len(model.ModelVersions) == 0 {
					log.Warnf("Model %s (%d) has no versions listed to process.", model.Name, model.ID)
					continue // Skip this model
				}
				versionsToProcess = model.ModelVersions
			} else {
				// Find the latest version if not downloading all
				latestVersion := models.ModelVersion{}
				latestTime := time.Time{}
				if len(model.ModelVersions) == 0 {
					log.Warnf("Model %s (%d) has no versions listed to process.", model.Name, model.ID)
					continue // Skip this model
				}
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
				if latestVersion.ID == 0 {
					log.Warnf("No valid latest version found for model %s (%d). Skipping.", model.Name, model.ID)
					continue // Skip this model
				}
				log.Debugf("Processing latest version %s (%d) for model %s (%d).", latestVersion.Name, latestVersion.ID, model.Name, model.ID)
				versionsToProcess = append(versionsToProcess, latestVersion)
			}

			// --- Loop through selected versions and process files ---
			for _, currentVersion := range versionsToProcess {
				log.Debugf("Processing files for version %s (%d) of model %s (%d)", currentVersion.Name, currentVersion.ID, model.Name, model.ID)
				// --- Filter by ignored base models --- (Case-Insensitive)
				ignoredBaseModels := viper.GetStringSlice("ignorebasemodels") // Use Viper
				if len(ignoredBaseModels) > 0 {
					versionBaseModelLower := strings.ToLower(currentVersion.BaseModel)
					for _, ignoreBaseModel := range ignoredBaseModels {
						if ignoreBaseModel != "" && strings.Contains(versionBaseModelLower, strings.ToLower(ignoreBaseModel)) {
							log.Debugf("    - Skipping version %s: Base model '%s' contains ignored string '%s'.", currentVersion.Name, currentVersion.BaseModel, ignoreBaseModel)
							continue // Skip to next version
						}
					}
				}

				// Prepare cleaned version for metadata/DB
				versionWithoutFilesImages := currentVersion
				versionWithoutFilesImages.Files = nil
				versionWithoutFilesImages.Images = nil

			fileLoop: // Label for continue
				for _, file := range currentVersion.Files { // Use files from currentVersion
					// Use the shared filtering function
					if !passesFileFilters(file, model.Type) {
						continue fileLoop // Skip this file if it doesn't pass filters
					}

					// --- Path/Filename Construction (using currentVersion) ---
					var slug string // Now only used for file path
					modelTypeName := helpers.ConvertToSlug(model.Type)
					baseModelStr := currentVersion.BaseModel // Use currentVersion
					if baseModelStr == "" {
						baseModelStr = "unknown-base"
					}
					baseModelSlug := helpers.ConvertToSlug(baseModelStr)
					modelNameSlug := helpers.ConvertToSlug(model.Name)
					// --- Modify slug construction for type/model/base structure ---
					slug = filepath.Join(modelTypeName, modelNameSlug, baseModelSlug) // Changed order for file path
					// --- End slug construction modification ---

					// --- Create version specific slug based on file name (without extension) ---
					fileNameWithoutExt := strings.TrimSuffix(file.Name, filepath.Ext(file.Name))
					versionSlug := fmt.Sprintf("%d-%s", currentVersion.ID, helpers.ConvertToSlug(fileNameWithoutExt))
					// --- End version specific slug ---

					baseFileName := helpers.ConvertToSlug(file.Name)
					ext := filepath.Ext(baseFileName)
					baseFileName = strings.TrimSuffix(baseFileName, ext)
					if strings.ToLower(file.Metadata.Format) == "safetensor" && !strings.EqualFold(ext, ".safetensors") {
						ext = ".safetensors"
					}
					if ext == "" {
						ext = ".bin"
						log.Warnf("File %s in version %s (%d) has no extension, defaulting to '.bin'", file.Name, currentVersion.Name, currentVersion.ID)
					}
					finalBaseFilenameOnly := baseFileName + ext
					constructedFileNameOnly := baseFileName + ext // Just base + extension
					// --- Modify directory path to include version ---
					fullDirPath := filepath.Join(cfg.SavePath, slug, versionSlug)
					// --- End directory path modification ---
					fullFilePath := filepath.Join(fullDirPath, constructedFileNameOnly) // Use filename without suffix
					// --- End Path/Filename Construction ---

					// Create potentialDownload using currentVersion data
					pd := potentialDownload{
						ModelName:         model.Name,
						ModelType:         model.Type,
						VersionName:       currentVersion.Name,      // Use currentVersion
						BaseModel:         currentVersion.BaseModel, // Use currentVersion
						Creator:           model.Creator,
						File:              file,
						ModelVersionID:    currentVersion.ID, // Use currentVersion
						TargetFilepath:    fullFilePath,      // Path without suffix
						Slug:              slug,
						FinalBaseFilename: finalBaseFilenameOnly,     // Keep original base+ext for reference
						CleanedVersion:    versionWithoutFilesImages, // Use cleaned currentVersion
						FullVersion:       currentVersion,            // Store the full original version data
						OriginalImages:    currentVersion.Images,     // Use currentVersion images
					}
					potentialDownloadsThisPage = append(potentialDownloadsThisPage, pd)
					// Log the intended path *without* suffix for clarity in this phase
					log.Debugf("Passed filters: %s (Model: %s (%d), Version: %s (%d)) -> %s", file.Name, model.Name, model.ID, currentVersion.Name, currentVersion.ID, fullFilePath)
				} // End fileLoop
			} // --- End version loop ---

			// --- Process this page's potential downloads against the DB ---
			log.Debugf("Checking %d potential downloads from page %d against database...", len(potentialDownloadsThisPage), pageCount)
			// Assuming processPage is available after refactoring
			queuedFromPage, sizeFromPage := processPage(db, potentialDownloadsThisPage, cfg)
			if len(queuedFromPage) > 0 {
				allPotentialDownloads = append(allPotentialDownloads, queuedFromPage...)
				totalQueuedSizeBytes += sizeFromPage
				log.Infof("Queued %d file(s) (Size: %s) from page %d after DB check.", len(queuedFromPage), helpers.BytesToSize(sizeFromPage), pageCount)
			} else {
				log.Debugf("No new files queued from page %d after DB check.", pageCount)
			}

			// --- Check Total Limit --- START ---
			if userTotalLimit > 0 && totalModelsReceived >= userTotalLimit {
				log.Infof("Reached total model limit (%d). Stopping pagination.", userTotalLimit)
				break // Stop fetching more pages
			}
			// --- Check Total Limit --- END ---

			// --- Stop if no cursor --- (Moved after total limit check)
			if nextCursor == "" {
				break // No more pages
			}
		} // End model loop for this page

		// --- Check Total Limit --- START ---
		if userTotalLimit > 0 && totalModelsReceived >= userTotalLimit {
			log.Infof("Reached total model limit (%d). Stopping pagination.", userTotalLimit)
			break // Stop fetching more pages
		}
		// --- Check Total Limit --- END ---

		// --- Stop if no cursor --- (Moved after total limit check)
		if nextCursor == "" {
			break // No more pages
		}

		// Apply API delay using Viper value
		if apiDelayMs > 0 {
			log.Debugf("Waiting %dms before next API request...", apiDelayMs)
			time.Sleep(time.Duration(apiDelayMs) * time.Millisecond)
		}
	}

	log.Infof("Finished fetching all pages. Received %d models total from API.", totalModelsReceived)
	return allPotentialDownloads, totalQueuedSizeBytes, nil
}

// min is a helper function to find the minimum of two integers.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
