package cmd

import "go-civitai-download/internal/models"

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
	OriginalImages []models.ModelImage // Add original images for potential download
}

// Represents a download task to be processed by a worker.
type downloadJob struct {
	PotentialDownload potentialDownload // Embed potential download info
	DatabaseKey       string            // Key for DB updates
}
