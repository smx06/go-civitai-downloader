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
	"github.com/spf13/cobra" // Added for cmd parameter
	"github.com/spf13/viper"
)

// --- Function Moved from download.go ---
// handleSingleVersionDownload Fetches details for a specific model version ID and processes it for download.
func handleSingleVersionDownload(versionID int, db *database.DB, client *http.Client, cfg *models.Config, cmd *cobra.Command) ([]potentialDownload, uint64, error) {
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
		if cfg.GetOnlyPrimaryModel && !file.Primary {
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
			if cfg.GetPruned && !strings.EqualFold(sizeStr, "pruned") {
				log.Debugf("Skipping non-pruned file %s (Size: %s) in checkpoint model version %d.", file.Name, sizeStr, versionID)
				continue
			}
			if cfg.GetFp16 && !strings.EqualFold(fpStr, "fp16") {
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
func fetchModelsPaginated(db *database.DB, client *http.Client, queryParams models.QueryParameters, cfg *models.Config, cmd *cobra.Command) ([]potentialDownload, uint64, error) {
	var allDownloadsToQueue []potentialDownload
	var totalQueuedSizeBytes uint64 = 0
	var loopErr error
	nextCursor := ""
	pageCount := 0
	maxPages, _ := cmd.Flags().GetInt("max-pages") // Get maxPages flag
	// imageDownloader needs to be passed in or created based on global config/flags
	// Placeholder: Create it here for now
	var imageDownloader *downloader.Downloader
	if viper.GetBool("download.save_model_images") {
		// Determine concurrency for image downloader
		imgDLConcurrency := viper.GetInt("download.concurrency") // Reuse main concurrency?
		if imgDLConcurrency <= 0 {
			imgDLConcurrency = 4
		}
		imageDownloader = downloader.NewDownloader(createDownloaderClient(imgDLConcurrency), cfg.ApiKey)
	}

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

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			loopErr = fmt.Errorf("failed to create request (Page %d): %w", pageCount, err)
			break
		}
		if cfg.ApiKey != "" {
			req.Header.Add("Authorization", "Bearer "+cfg.ApiKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			if urlErr, ok := err.(*url.Error); ok && urlErr.Timeout() {
				log.WithError(err).Warnf("Timeout fetching metadata page %d. Retrying after delay...", pageCount)
				time.Sleep(5 * time.Second)
				// loopErr = fmt.Errorf("timeout fetching metadata page %d: %w", pageCount, err)
				// Instead of breaking, let's just continue to the next polite delay? Or should we retry?
				// Continuing might skip a page on persistent timeout. Breaking is safer without proper retry.
				loopErr = fmt.Errorf("timeout fetching metadata page %d: %w", pageCount, err)
				break // Breaking for now
			}
			loopErr = fmt.Errorf("failed to fetch metadata (Page %d): %w", pageCount, err)
			break
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.WithError(closeErr).Warn("Error closing response body after reading")
		}

		if readErr != nil {
			log.WithError(readErr).Errorf("Error reading response body for page %d, status %s", pageCount, resp.Status)
			loopErr = fmt.Errorf("failed to read response body (Page %d): %w", pageCount, readErr)
			break
		}

		if resp.StatusCode != http.StatusOK {
			errMsg := fmt.Sprintf("API request failed (Page %d) with status %s", pageCount, resp.Status)
			if len(bodyBytes) > 0 {
				maxLen := 200
				bodyStr := string(bodyBytes)
				if len(bodyStr) > maxLen {
					bodyStr = bodyStr[:maxLen] + "..."
				}
				errMsg += fmt.Sprintf(". Response: %s", bodyStr)
			}
			log.Error(errMsg)
			if resp.StatusCode == http.StatusUnauthorized && cfg.ApiKey != "" {
				errMsg += ". Check if your Civitai API Key is correct/valid."
				log.Error(errMsg)
			} else if resp.StatusCode == http.StatusTooManyRequests {
				errMsg += ". Rate limited. Applying longer delay..."
				log.Warn(errMsg)
				// Use cfg.ApiDelayMs for base delay calculation
				delay := time.Duration(cfg.ApiDelayMs)*time.Millisecond*5 + 5*time.Second
				log.Warnf("Applying rate limit delay: %v", delay)
				time.Sleep(delay)
				// TODO: Consider retry logic here instead of just breaking.
				// For now, just continue to let the outer loop handle delay? No, break.
				loopErr = fmt.Errorf(errMsg)
				break // Breaking for now
			}
			loopErr = fmt.Errorf(errMsg)
			break
		}

		var response models.ApiResponse
		if err := json.Unmarshal(bodyBytes, &response); err != nil {
			loopErr = fmt.Errorf("failed to decode API response (Page %d): %w", pageCount, err)
			log.WithError(err).Errorf("Response body sample: %s", string(bodyBytes[:min(len(bodyBytes), 200)]))
			break
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
			// --- End Version Selection ---

			// --- Save Full Model Info if Flag is Set ---
			saveFullInfo, _ := cmd.Flags().GetBool("save-model-info")
			if saveFullInfo {
				// --- Validation for --save-model-images ---
				saveModelImages, _ := cmd.Flags().GetBool("save-model-images")
				if saveModelImages && !saveFullInfo {
					log.Error("--save-model-images requires --save-model-info to be set as well. Aborting image download for this model.")
					saveModelImages = false
				}
				// --- End Validation ---

				modelNameSlug := helpers.ConvertToSlug(model.Name)
				if modelNameSlug == "" {
					modelNameSlug = "unknown_model"
				}
				baseModelSlug := "unknown_base_model"
				if latestVersion.ID != 0 {
					baseModelSlug = helpers.ConvertToSlug(latestVersion.BaseModel)
					if baseModelSlug == "" {
						baseModelSlug = "unknown_base_model"
					}
				}

				if err := saveModelInfoFile(model, cfg.SavePath, baseModelSlug, modelNameSlug); err != nil {
					log.WithError(err).Warnf("Failed to save full model info for model %d (%s)", model.ID, model.Name)
				}

				// --- Download Model Images if Enabled ---
				if saveModelImages {
					log.Infof("Downloading all model images for %s (%d)...", model.Name, model.ID)
					if imageDownloader == nil {
						log.Warn("Image downloader not available for save-model-images. Skipping image downloads.")
					} else {
						modelImagesBaseDir := filepath.Join(cfg.SavePath, "model_info", baseModelSlug, modelNameSlug, "images")
						var modelImagesDownloaded, modelImagesSkipped, modelImagesFailed int = 0, 0, 0

						for _, version := range model.ModelVersions {
							versionImagesDir := filepath.Join(modelImagesBaseDir, fmt.Sprintf("%d", version.ID))
							if len(version.Images) > 0 {
								if err := os.MkdirAll(versionImagesDir, 0755); err != nil {
									log.WithError(err).Errorf("Failed to create directory for model images: %s", versionImagesDir)
									modelImagesFailed += len(version.Images)
									continue
								}
								for imgIdx, image := range version.Images {
									imgUrlParsed, urlErr := url.Parse(image.URL)
									var imgFilename string
									if urlErr != nil || image.ID == 0 {
										log.WithError(urlErr).Warnf("Cannot determine filename/ID for model image %d (URL: %s). Using index.", imgIdx, image.URL)
										imgFilename = fmt.Sprintf("image_%d.jpg", imgIdx)
									} else {
										ext := filepath.Ext(imgUrlParsed.Path)
										if ext == "" {
											ext = ".jpg"
										}
										imgFilename = fmt.Sprintf("%d%s", image.ID, ext)
									}
									imgTargetPath := filepath.Join(versionImagesDir, imgFilename)

									if _, statErr := os.Stat(imgTargetPath); statErr == nil {
										log.Debugf("Skipping model image %s - already exists.", imgFilename)
										modelImagesSkipped++
										continue
									}

									log.Debugf("Downloading model image %s to %s", image.URL, imgTargetPath)
									_, dlErr := imageDownloader.DownloadFile(imgTargetPath, image.URL, models.Hashes{}, 0)
									if dlErr != nil {
										log.WithError(dlErr).Errorf("Failed to download model image %s", imgFilename)
										modelImagesFailed++
									} else {
										modelImagesDownloaded++
									}
								}
							}
						}
						log.Infof("Finished model images for %s (%d): Downloaded: %d, Skipped: %d, Failed: %d",
							model.Name, model.ID, modelImagesDownloaded, modelImagesSkipped, modelImagesFailed)
					}
				}
				// --- End Download Model Images ---
			}
			// --- End Save Full Model Info ---

			if latestVersion.ID == 0 {
				log.Debugf("Skipping model %s (%d) - no valid versions found for further processing.", model.Name, model.ID)
				continue
			}

			// --- Filtering Logic (Model Level) ---
			if len(cfg.IgnoreBaseModels) > 0 {
				ignore := false
				for _, ignoreBaseModel := range cfg.IgnoreBaseModels {
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

			versionWithoutFilesImages := latestVersion
			versionWithoutFilesImages.Files = nil
			versionWithoutFilesImages.Images = nil

		fileLoop:
			for _, file := range latestVersion.Files {
				// --- Filtering Logic (File Level) ---
				if file.Hashes.CRC32 == "" {
					log.Debugf("Skipping file %s in model %s (%d): Missing CRC32 hash.", file.Name, model.Name, model.ID)
					continue
				}
				if cfg.GetOnlyPrimaryModel && !file.Primary {
					log.Debugf("Skipping non-primary file %s in model %s (%d).", file.Name, model.Name, model.ID)
					continue
				}
				if file.Metadata.Format == "" {
					log.Debugf("Skipping file %s in model %s (%d): Missing metadata format.", file.Name, model.Name, model.ID)
					continue
				}
				if strings.ToLower(file.Metadata.Format) != "safetensor" {
					log.Debugf("Skipping non-safetensor file %s (Format: %s) in model %s (%d).", file.Name, file.Metadata.Format, model.Name, model.ID)
					continue
				}
				if strings.EqualFold(model.Type, "checkpoint") {
					sizeStr := fmt.Sprintf("%v", file.Metadata.Size)
					fpStr := fmt.Sprintf("%v", file.Metadata.Fp)
					if cfg.GetPruned && !strings.EqualFold(sizeStr, "pruned") {
						log.Debugf("Skipping non-pruned file %s (Size: %s) in checkpoint model %s (%d).", file.Name, sizeStr, model.Name, model.ID)
						continue
					}
					if cfg.GetFp16 && !strings.EqualFold(fpStr, "fp16") {
						log.Debugf("Skipping non-fp16 file %s (FP: %s) in checkpoint model %s (%d).", file.Name, fpStr, model.Name, model.ID)
						continue
					}
				}
				if len(cfg.IgnoreFileNameStrings) > 0 {
					for _, ignoreFileName := range cfg.IgnoreFileNameStrings {
						if strings.Contains(strings.ToLower(file.Name), strings.ToLower(ignoreFileName)) {
							log.Debugf("Skipping file %s in model %s (%d) due to ignored filename string '%s'.", file.Name, model.Name, model.ID, ignoreFileName)
							continue fileLoop
						}
					}
				}
				// --- End Filtering Logic ---

				// --- Path/Filename Construction ---
				var slug string
				modelTypeName := helpers.ConvertToSlug(model.Type)
				baseModelStr := latestVersion.BaseModel
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
					log.Warnf("File %s in model %s (%d) has no extension, defaulting to '.bin'", file.Name, model.Name, model.ID)
				}
				finalBaseFilenameOnly := baseFileName + ext
				dbKeySimple := strings.ToUpper(file.Hashes.CRC32)
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
				constructedFileNameWithSuffix := baseFileName + metaSuffix + ext
				fullDirPath := filepath.Join(cfg.SavePath, slug)
				fullFilePath := filepath.Join(fullDirPath, constructedFileNameWithSuffix)
				// --- End Path/Filename Construction ---

				pd := potentialDownload{
					ModelName:         model.Name,
					ModelType:         model.Type,
					VersionName:       latestVersion.Name,
					BaseModel:         latestVersion.BaseModel,
					Creator:           model.Creator,
					File:              file,
					ModelVersionID:    latestVersion.ID,
					TargetFilepath:    fullFilePath,
					Slug:              slug,
					FinalBaseFilename: finalBaseFilenameOnly,
					CleanedVersion:    versionWithoutFilesImages,
					OriginalImages:    latestVersion.Images,
				}
				potentialDownloadsThisPage = append(potentialDownloadsThisPage, pd)
				log.Debugf("Passed filters: %s (Model: %s (%d)) -> %s", file.Name, model.Name, model.ID, fullFilePath)
			} // End file loop
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

	if loopErr != nil {
		log.Errorf("Metadata gathering phase finished with error: %v", loopErr)
		// Return the error so the caller (runDownload) can decide how to proceed
		return allDownloadsToQueue, totalQueuedSizeBytes, loopErr
	}
	log.Info("--- Finished Paginated Model Fetch ---")

	return allDownloadsToQueue, totalQueuedSizeBytes, nil // Return collected downloads and nil error
}

// Helper function
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
