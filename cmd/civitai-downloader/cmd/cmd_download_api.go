package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"go-civitai-download/internal/database"
	"go-civitai-download/internal/downloader"
	"go-civitai-download/internal/helpers"
	"go-civitai-download/internal/models"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// handleSingleVersionDownload Fetches details for a specific model version ID and processes it for download.
func handleSingleVersionDownload(versionID int, db *database.DB, client *http.Client, cfg *models.Config, _ *cobra.Command) ([]potentialDownload, uint64, error) {
	log.Debugf("Fetching details for model version ID: %d", versionID)
	apiURL := fmt.Sprintf("https://civitai.com/api/v1/model-versions/%d", versionID)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request for version %d: %w", versionID, err)
	}
	if cfg.ApiKey != "" {
		req.Header.Add("Authorization", "Bearer "+cfg.ApiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Handle timeout specifically?
		return nil, 0, fmt.Errorf("failed to fetch version %d: %w", versionID, err)
	}
	defer resp.Body.Close()

	bodyBytes, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, 0, fmt.Errorf("failed to read response body for version %d: %w", versionID, readErr)
	}

	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Sprintf("API request failed for version %d with status %s", versionID, resp.Status)
		if len(bodyBytes) > 0 {
			maxLen := 200
			bodyStr := string(bodyBytes)
			if len(bodyStr) > maxLen {
				bodyStr = bodyStr[:maxLen] + "..."
			}
			errMsg += fmt.Sprintf(". Response: %s", bodyStr)
		}
		return nil, 0, fmt.Errorf(errMsg)
	}

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
		// --- Filtering Logic (File Level - Copied/adapted from pagination loop) ---
		if file.Hashes.CRC32 == "" {
			log.Debugf("Skipping file %s in model version %d: Missing CRC32 hash.", file.Name, versionID)
			continue
		}
		if cfg.PrimaryOnly && !file.Primary {
			log.Debugf("Skipping non-primary file %s in model version %d.", file.Name, versionID)
			continue
		}
		if file.Metadata.Format == "" {
			log.Debugf("Skipping file %s in model version %d: Missing metadata format.", file.Name, versionID)
			continue
		}
		if strings.ToLower(file.Metadata.Format) != "safetensor" {
			log.Debugf("Skipping non-safetensor file %s (Format: %s) in model version %d.", file.Name, file.Metadata.Format, versionID)
			continue
		}
		if strings.EqualFold(versionResponse.Model.Type, "checkpoint") {
			sizeStr := fmt.Sprintf("%v", file.Metadata.Size)
			fpStr := fmt.Sprintf("%v", file.Metadata.Fp)
			if cfg.Pruned && !strings.EqualFold(sizeStr, "pruned") {
				log.Debugf("Skipping non-pruned file %s (Size: %s) in checkpoint model version %d.", file.Name, sizeStr, versionID)
				continue
			}
			if cfg.Fp16 && !strings.EqualFold(fpStr, "fp16") {
				log.Debugf("Skipping non-fp16 file %s (FP: %s) in checkpoint model version %d.", file.Name, fpStr, versionID)
				continue
			}
		}
		if len(cfg.IgnoreFileNameStrings) > 0 {
			for _, ignoreFileName := range cfg.IgnoreFileNameStrings {
				if strings.Contains(strings.ToLower(file.Name), strings.ToLower(ignoreFileName)) {
					log.Debugf("Skipping file %s in model version %d due to ignored filename string '%s'.", file.Name, versionID, ignoreFileName)
					continue // Check next file in this version
				}
			}
		}
		// --- End Filtering Logic ---

		// --- Path/Filename Construction (Copied/adapted from pagination loop) ---
		var slug string
		modelTypeName := helpers.ConvertToSlug(versionResponse.Model.Type)
		baseModelStr := versionResponse.BaseModel
		if baseModelStr == "" {
			baseModelStr = "unknown-base"
		}
		baseModelSlug := helpers.ConvertToSlug(baseModelStr)
		modelNameSlug := helpers.ConvertToSlug(versionResponse.Model.Name)
		if !strings.EqualFold(versionResponse.Model.Type, "checkpoint") {
			slug = filepath.Join(modelTypeName+"-"+baseModelSlug, modelNameSlug)
		} else {
			slug = filepath.Join(baseModelSlug, modelNameSlug)
		}
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
		fullDirPath := filepath.Join(cfg.SavePath, slug)
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

// fetchModelsPaginated handles the process of fetching models using API pagination.
func fetchModelsPaginated(db *database.DB, client *http.Client, imageDownloader *downloader.Downloader, queryParams models.QueryParameters, cfg *models.Config, cmd *cobra.Command) ([]potentialDownload, uint64, error) {
	var allDownloadsToQueue []potentialDownload
	var totalQueuedSizeBytes uint64 = 0
	var loopErr error
	nextCursor := ""
	pageCount := 0
	maxPages, _ := cmd.Flags().GetInt("max-pages") // Get maxPages flag
	maxRetries := 3                                // Max retries for API calls
	baseRetryDelay := 2 * time.Second              // Base delay for retries

	log.Info("--- Starting Paginated Model Fetch ---")

	// --- Start of Moved Pagination Loop ---
	for {
		pageCount++
		if maxPages > 0 && pageCount > maxPages {
			log.Infof("Reached max pages limit (%d). Stopping pagination.", maxPages)
			break
		}

		currentParams := queryParams // Start with base params for this page
		if nextCursor != "" {
			currentParams.Cursor = nextCursor
		}

		apiURL := models.ConstructApiUrl(currentParams)
		log.Debugf("Requesting URL (Page %d): %s", pageCount, apiURL)

		var resp *http.Response
		var reqErr error
		var bodyBytes []byte // Declare bodyBytes *before* the retry loop

		// --- Retry Loop for API Request ---
		for attempt := 1; attempt <= maxRetries; attempt++ {
			var currentResp *http.Response // Response for this specific attempt

			req, err := http.NewRequest("GET", apiURL, nil)
			if err != nil {
				loopErr = fmt.Errorf("failed to create request (Page %d): %w", pageCount, err)
				goto endLoop // Use goto to break out of nested loops/retries
			}
			if cfg.ApiKey != "" {
				req.Header.Add("Authorization", "Bearer "+cfg.ApiKey)
			}

			currentResp, reqErr = client.Do(req) // Assign to currentResp

			if reqErr != nil {
				// Close body if response exists, even on error
				if currentResp != nil && currentResp.Body != nil {
					currentResp.Body.Close()
				}
				if urlErr, ok := reqErr.(*url.Error); ok && urlErr.Timeout() {
					log.WithError(reqErr).Warnf("Timeout fetching metadata page %d (Attempt %d/%d). Retrying after delay...", pageCount, attempt, maxRetries)
					if attempt < maxRetries {
						time.Sleep(baseRetryDelay * time.Duration(attempt)) // Exponential backoff
						continue                                            // Retry request
					}
				}
				// Non-timeout error or final attempt failed
				loopErr = fmt.Errorf("failed to fetch metadata (Page %d, Attempt %d): %w", pageCount, attempt, reqErr)
				goto endLoop
			}

			// --- Status Code Checks ---
			if currentResp.StatusCode == http.StatusOK {
				// Successful response, read the body and break retry loop
				var readErr error
				bodyBytes, readErr = io.ReadAll(currentResp.Body) // Read into outer bodyBytes
				if closeErr := currentResp.Body.Close(); closeErr != nil {
					log.WithError(closeErr).Warn("Error closing response body after successful read")
				}
				if readErr != nil {
					loopErr = fmt.Errorf("failed to read response body after status OK (Page %d): %w", pageCount, readErr)
					goto endLoop
				}
				// Check for empty body after successful read
				if len(bodyBytes) == 0 {
					log.Warnf("API returned 200 OK but with empty body for page %d. Assuming end of results.", pageCount)
					goto endLoop // Treat as end of pagination
				}
				resp = currentResp // Assign successful response to outer resp
				break              // Success, exit retry loop
			}

			// Handle retryable errors (e.g., 429)
			if currentResp.StatusCode == http.StatusTooManyRequests {
				log.Warnf("Rate limited (429) fetching page %d (Attempt %d/%d). Retrying after longer delay...", pageCount, attempt, maxRetries)
				// MUST close body before continuing/sleeping
				if closeErr := currentResp.Body.Close(); closeErr != nil {
					log.WithError(closeErr).Warn("Error closing response body before rate limit retry delay")
				}
				if attempt < maxRetries {
					delay := baseRetryDelay*time.Duration(attempt)*2 + 5*time.Second // Longer backoff for rate limits
					log.Warnf("Applying rate limit delay: %v", delay)
					time.Sleep(delay)
					continue // Retry request
				} else {
					// Final attempt failed due to rate limit
					loopErr = fmt.Errorf("API request failed (Page %d, Attempt %d) due to rate limit (429)", pageCount, attempt)
					goto endLoop
				}
			}

			// Handle other non-OK, non-retryable status codes
			errMsg := fmt.Sprintf("API request failed (Page %d) with status %s", pageCount, currentResp.Status)
			// Read body only for error logging if necessary
			errorBodyBytes, _ := io.ReadAll(currentResp.Body)
			if closeErr := currentResp.Body.Close(); closeErr != nil {
				log.WithError(closeErr).Warn("Error closing response body after non-OK status")
			}
			if len(errorBodyBytes) > 0 {
				maxLen := 200
				bodyStr := string(errorBodyBytes)
				if len(bodyStr) > maxLen {
					bodyStr = bodyStr[:maxLen] + "..."
				}
				errMsg += fmt.Sprintf(". Response: %s", bodyStr)
			}
			log.Error(errMsg)
			if currentResp.StatusCode == http.StatusUnauthorized && cfg.ApiKey != "" {
				errMsg += ". Check if your Civitai API Key is correct/valid."
				log.Error(errMsg) // Log again with extra info
			}
			loopErr = errors.New(errMsg)
			goto endLoop // Break outer loop for non-retryable errors

		} // --- End Retry Loop ---

		// If resp is nil here, it means all retries failed without setting loopErr properly (shouldn't happen ideally)
		if resp == nil {
			loopErr = fmt.Errorf("internal error: response is nil after retry loop without specific error (Page %d)", pageCount)
			break
		}

		// --- Body was read successfully within the loop, now unmarshal ---
		var response models.ApiResponse
		// Log the length of the received body before attempting to unmarshal
		log.Debugf("Received %d bytes for API response body (Page %d). Attempting to unmarshal...", len(bodyBytes), pageCount)

		if err := json.Unmarshal(bodyBytes, &response); err != nil {
			// Include body snippet directly in the error log
			bodySnippet := string(bodyBytes[:min(len(bodyBytes), 500)]) // Increased snippet size
			// Make error more specific about potential truncation
			loopErr = fmt.Errorf("failed to decode API response JSON (Page %d, Received %d bytes, Body Snippet: '%s'): %w", pageCount, len(bodyBytes), bodySnippet, err)
			log.WithError(loopErr).Error("Error decoding API JSON response - check body snippet for potential truncation or malformed JSON")
			break // Break outer loop
		}

		if response.Metadata.NextCursor != "" {
			nextCursor = response.Metadata.NextCursor
			log.Debugf("API Metadata: TotalItems=%d, CurrentPage=%d, PageSize=%d, NextCursor=%s",
				response.Metadata.TotalItems, response.Metadata.CurrentPage, response.Metadata.PageSize, response.Metadata.NextCursor)
		} else {
			log.Warn("API response missing next cursor.")
			nextCursor = "" // Stop loop
		}

		// --- Process Models from this Page ---
		var potentialDownloadsThisPage []potentialDownload
		log.Debugf("Processing %d models from request %d for potential downloads...", len(response.Items), pageCount)

		for _, model := range response.Items {
			// --- Save Full Model Info / Images if Flag is Set ---
			// This logic runs regardless of which versions are downloaded later
			saveFullInfo, _ := cmd.Flags().GetBool("model-info")
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

				if err := saveModelInfoFile(model, cfg.SavePath, baseModelSlug, modelNameSlug); err != nil {
					log.WithError(err).Warnf("Failed to save full model info for model %d (%s)", model.ID, model.Name)
				}

				saveModelImages, _ := cmd.Flags().GetBool("model-images")
				if saveModelImages {
					if !saveFullInfo {
						log.Error("--model-images requires --model-info to be set as well. Aborting image download.")
					} else {
						logPrefix := fmt.Sprintf("Model %d Img", model.ID)
						log.Infof("[%s] Processing all model images for %s (%d)...", logPrefix, model.Name, model.ID)
						if imageDownloader == nil {
							log.Warnf("[%s] Image downloader not available for save-model-images. Skipping image downloads.", logPrefix)
						} else {
							modelImagesBaseDir := filepath.Join(cfg.SavePath, "model_info", baseModelSlug, modelNameSlug, "images")
							var totalImgSuccess, totalImgFail int = 0, 0
							for _, version := range model.ModelVersions {
								versionLogPrefix := fmt.Sprintf("%s v%d", logPrefix, version.ID)
								versionImagesDir := filepath.Join(modelImagesBaseDir, fmt.Sprintf("%d", version.ID))
								log.Debugf("[%s] Checking %d images for version %s (%d)", versionLogPrefix, len(version.Images), version.Name, version.ID)
								if len(version.Images) > 0 {
									log.Debugf("[%s] Calling downloadImages for %d images...", versionLogPrefix, len(version.Images))
									// Pass concurrencyLevel (obtained from cmd flags earlier)
									concurrency, _ := cmd.Flags().GetInt("concurrency") // Re-get concurrency
									if concurrency <= 0 {
										concurrency = 4
									} // Simple default if flag missing/invalid
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
				}
			} // --- End Save Full Model Info / Images ---

			// --- Version Selection / Processing ---
			downloadAll, _ := cmd.Flags().GetBool("all-versions")
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
				// --- Model-Level Filtering (applied to currentVersion) ---
				if len(cfg.IgnoreBaseModels) > 0 {
					ignore := false
					for _, ignoreBaseModel := range cfg.IgnoreBaseModels {
						if strings.Contains(strings.ToLower(currentVersion.BaseModel), strings.ToLower(ignoreBaseModel)) {
							log.Debugf("Skipping version %s (%d) of model %s due to ignored base model '%s'.", currentVersion.Name, currentVersion.ID, model.Name, ignoreBaseModel)
							ignore = true
							break
						}
					}
					if ignore {
						continue // Skip to next version
					}
				}
				// --- End Model-Level Filtering ---

				// Prepare cleaned version for metadata/DB
				versionWithoutFilesImages := currentVersion
				versionWithoutFilesImages.Files = nil
				versionWithoutFilesImages.Images = nil

			fileLoop: // Label for continue
				for _, file := range currentVersion.Files { // Use files from currentVersion
					// --- Filtering Logic (File Level) ---
					if file.Hashes.CRC32 == "" {
						log.Debugf("Skipping file %s in version %s (%d): Missing CRC32 hash.", file.Name, currentVersion.Name, currentVersion.ID)
						continue
					}
					if cfg.PrimaryOnly && !file.Primary {
						log.Debugf("Skipping non-primary file %s in version %s (%d).", file.Name, currentVersion.Name, currentVersion.ID)
						continue
					}
					if file.Metadata.Format == "" {
						log.Debugf("Skipping file %s in version %s (%d): Missing metadata format.", file.Name, currentVersion.Name, currentVersion.ID)
						continue
					}
					if strings.ToLower(file.Metadata.Format) != "safetensor" {
						log.Debugf("Skipping non-safetensor file %s (Format: %s) in version %s (%d).", file.Name, file.Metadata.Format, currentVersion.Name, currentVersion.ID)
						continue
					}
					if strings.EqualFold(model.Type, "checkpoint") {
						sizeStr := fmt.Sprintf("%v", file.Metadata.Size)
						fpStr := fmt.Sprintf("%v", file.Metadata.Fp)
						if cfg.Pruned && !strings.EqualFold(sizeStr, "pruned") {
							log.Debugf("Skipping non-pruned file %s (Size: %s) in checkpoint version %s (%d).", file.Name, sizeStr, currentVersion.Name, currentVersion.ID)
							continue
						}
						if cfg.Fp16 && !strings.EqualFold(fpStr, "fp16") {
							log.Debugf("Skipping non-fp16 file %s (FP: %s) in checkpoint version %s (%d).", file.Name, fpStr, currentVersion.Name, currentVersion.ID)
							continue
						}
					}
					if len(cfg.IgnoreFileNameStrings) > 0 {
						for _, ignoreFileName := range cfg.IgnoreFileNameStrings {
							if strings.Contains(strings.ToLower(file.Name), strings.ToLower(ignoreFileName)) {
								log.Debugf("Skipping file %s in version %s (%d) due to ignored filename string '%s'.", file.Name, currentVersion.Name, currentVersion.ID, ignoreFileName)
								continue fileLoop
							}
						}
					}
					// --- End Filtering Logic ---

					// --- Path/Filename Construction (using currentVersion) ---
					var slug string
					modelTypeName := helpers.ConvertToSlug(model.Type)
					baseModelStr := currentVersion.BaseModel // Use currentVersion
					if baseModelStr == "" {
						baseModelStr = "unknown-base"
					}
					baseModelSlug := helpers.ConvertToSlug(baseModelStr)
					modelNameSlug := helpers.ConvertToSlug(model.Name)
					if !strings.EqualFold(model.Type, "checkpoint") {
						slug = filepath.Join(modelTypeName+"-"+baseModelSlug, modelNameSlug)
					} else {
						slug = filepath.Join(baseModelSlug, modelNameSlug)
					}
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
					fullDirPath := filepath.Join(cfg.SavePath, slug)
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
						OriginalImages:    currentVersion.Images,     // Use currentVersion images
					}
					potentialDownloadsThisPage = append(potentialDownloadsThisPage, pd)
					// Log the intended path *without* suffix for clarity in this phase
					log.Debugf("Passed filters: %s (Model: %s (%d), Version: %s (%d)) -> %s", file.Name, model.Name, model.ID, currentVersion.Name, currentVersion.ID, fullFilePath)
				} // End fileLoop
			} // --- End version loop ---

		} // End model loop for this page

		// --- Process this page's potential downloads against the DB ---
		log.Debugf("Checking %d potential downloads from page %d against database...", len(potentialDownloadsThisPage), pageCount)
		// Assuming processPage is available after refactoring
		queuedFromPage, sizeFromPage := processPage(db, potentialDownloadsThisPage, cfg)
		if len(queuedFromPage) > 0 {
			allDownloadsToQueue = append(allDownloadsToQueue, queuedFromPage...)
			totalQueuedSizeBytes += sizeFromPage
			log.Infof("Queued %d file(s) (Size: %s) from page %d after DB check.", len(queuedFromPage), helpers.BytesToSize(sizeFromPage), pageCount)
		} else {
			log.Debugf("No new files queued from page %d after DB check.", pageCount)
		}

		if nextCursor == "" {
			log.Info("Finished gathering metadata: No next cursor provided by API.")
			break
		}

		if cfg.ApiDelayMs > 0 {
			log.Debugf("Applying API delay: %d ms", cfg.ApiDelayMs)
			time.Sleep(time.Duration(cfg.ApiDelayMs) * time.Millisecond)
		}
	} // --- End of Moved Pagination Loop ---

endLoop: // Label for breaking out cleanly
	if loopErr != nil {
		log.Errorf("Exiting pagination loop due to error: %v", loopErr)
	}

	log.Info("--- Finished Paginated Model Fetch ---")

	return allDownloadsToQueue, totalQueuedSizeBytes, loopErr
}

// Helper function
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
