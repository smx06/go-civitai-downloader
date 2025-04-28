package models

import (
	"net/url"
	"strconv"
)

type (
	Config struct {
		// Connection/Auth
		ApiKey string `toml:"ApiKey"`

		// Paths
		SavePath       string `toml:"SavePath"`
		DatabasePath   string `toml:"DatabasePath"`
		BleveIndexPath string `toml:"BleveIndexPath"` // New field for Bleve index path

		// Filtering - Model/Version Level
		Query               string   `toml:"Query"`
		Tag                 string   `toml:"Tag"`
		Username            string   `toml:"Username"`
		ModelTypes          []string `toml:"ModelTypes"` // Renamed from Types
		BaseModels          []string `toml:"BaseModels"`
		IgnoreBaseModels    []string `toml:"IgnoreBaseModels"`
		Nsfw                bool     `toml:"Nsfw"`                // Renamed from GetNsfw
		ModelVersionID      int      `toml:"ModelVersionID"`      // New
		DownloadAllVersions bool     `toml:"DownloadAllVersions"` // New

		// Filtering - File Level
		PrimaryOnly           bool     `toml:"PrimaryOnly"` // Renamed from GetOnlyPrimaryModel
		Pruned                bool     `toml:"Pruned"`      // Renamed from GetPruned
		Fp16                  bool     `toml:"Fp16"`        // Renamed from GetFp16
		IgnoreFileNameStrings []string `toml:"IgnoreFileNameStrings"`

		// API Query Behavior
		Sort     string `toml:"Sort"`
		Period   string `toml:"Period"`
		Limit    int    `toml:"Limit"`
		MaxPages int    `toml:"MaxPages"` // New

		// Downloader Behavior
		Concurrency         int  `toml:"Concurrency"` // Renamed from DefaultConcurrency
		SaveMetadata        bool `toml:"SaveMetadata"`
		DownloadMetaOnly    bool `toml:"DownloadMetaOnly"`  // New
		SaveModelInfo       bool `toml:"SaveModelInfo"`     // New
		SaveVersionImages   bool `toml:"SaveVersionImages"` // New
		SaveModelImages     bool `toml:"SaveModelImages"`   // New
		SkipConfirmation    bool `toml:"SkipConfirmation"`  // New (for --yes flag)
		ApiDelayMs          int  `toml:"ApiDelayMs"`
		ApiClientTimeoutSec int  `toml:"ApiClientTimeoutSec"`

		// Other
		LogApiRequests bool `toml:"LogApiRequests"`
	}

	// Api Calls and Responses
	QueryParameters struct {
		Limit                  int      `json:"limit"`
		Page                   int      `json:"page,omitempty"`
		Query                  string   `json:"query,omitempty"`
		Tag                    string   `json:"tag,omitempty"`
		Username               string   `json:"username,omitempty"`
		Types                  []string `json:"types,omitempty"`
		Sort                   string   `json:"sort"`
		Period                 string   `json:"period"`
		PrimaryFileOnly        bool     `json:"primaryFileOnly,omitempty"`
		AllowNoCredit          bool     `json:"allowNoCredit,omitempty"`
		AllowDerivatives       bool     `json:"allowDerivatives,omitempty"`
		AllowDifferentLicenses bool     `json:"allowDifferentLicenses,omitempty"`
		AllowCommercialUse     string   `json:"allowCommercialUse,omitempty"`
		Nsfw                   bool     `json:"nsfw"`
		BaseModels             []string `json:"baseModels,omitempty"`
		Cursor                 string   `json:"cursor,omitempty"`
	}

	Model struct {
		ID                    int            `json:"id"`
		Name                  string         `json:"name"`
		Description           string         `json:"description"`
		Type                  string         `json:"type"`
		Poi                   bool           `json:"poi"`
		Nsfw                  bool           `json:"nsfw"`
		AllowNoCredit         bool           `json:"allowNoCredit"`
		AllowCommercialUse    []string       `json:"allowCommercialUse"`
		AllowDerivatives      bool           `json:"allowDerivatives"`
		AllowDifferentLicense bool           `json:"allowDifferentLicense"`
		Stats                 Stats          `json:"stats"`
		Creator               Creator        `json:"creator"`
		Tags                  []string       `json:"tags"`
		ModelVersions         []ModelVersion `json:"modelVersions"`
		Meta                  interface{}    `json:"meta"` // Meta can be null or an object, so we use interface{}
	}

	Stats struct {
		DownloadCount int     `json:"downloadCount"`
		FavoriteCount int     `json:"favoriteCount"`
		CommentCount  int     `json:"commentCount"`
		RatingCount   int     `json:"ratingCount"`
		Rating        float64 `json:"rating"`
	}

	Creator struct {
		Username string `json:"username"`
		Image    string `json:"image"`
	}

	// --- NEW: Struct for nested 'model' field in /model-versions/{id} response ---
	BaseModelInfo struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Nsfw bool   `json:"nsfw"`
		Poi  bool   `json:"poi"`
		Mode string `json:"mode"` // Can be null, "Archived", "TakenDown"
	}

	ModelVersion struct {
		ID                   int          `json:"id"`
		ModelId              int          `json:"modelId"`
		Name                 string       `json:"name"`
		PublishedAt          string       `json:"publishedAt"`
		UpdatedAt            string       `json:"updatedAt"`
		TrainedWords         []string     `json:"trainedWords"`
		BaseModel            string       `json:"baseModel"`
		EarlyAccessTimeFrame int          `json:"earlyAccessTimeFrame"`
		Description          string       `json:"description"`
		Stats                Stats        `json:"stats"`
		Files                []File       `json:"files"`
		Images               []ModelImage `json:"images"`
		DownloadUrl          string       `json:"downloadUrl"`
		// --- ADDED: Nested model info from /model-versions/{id} endpoint ---
		Model BaseModelInfo `json:"model"`
	}

	File struct {
		Name              string   `json:"name"`
		ID                int      `json:"id"`
		SizeKB            float64  `json:"sizeKB"`
		Type              string   `json:"type"`
		Metadata          Metadata `json:"metadata"`
		PickleScanResult  string   `json:"pickleScanResult"`
		PickleScanMessage string   `json:"pickleScanMessage"`
		VirusScanResult   string   `json:"virusScanResult"`
		ScannedAt         string   `json:"scannedAt"`
		Hashes            Hashes   `json:"hashes"`
		DownloadUrl       string   `json:"downloadUrl"`
		Primary           bool     `json:"primary"`
	}

	Metadata struct {
		Fp     string `json:"fp"`
		Size   string `json:"size"`
		Format string `json:"format"`
	}

	Hashes struct {
		AutoV2 string `json:"AutoV2"`
		SHA256 string `json:"SHA256"`
		CRC32  string `json:"CRC32"`
		BLAKE3 string `json:"BLAKE3"`
	}

	ModelImage struct {
		ID        int         `json:"id"`
		URL       string      `json:"url"`
		Hash      string      `json:"hash"` // Blurhash
		Width     int         `json:"width"`
		Height    int         `json:"height"`
		Nsfw      bool        `json:"nsfw"`      // Keep boolean for simplicity, align with Model struct Nsfw
		NsfwLevel interface{} `json:"nsfwLevel"` // Changed to interface{} to handle number OR string from API
		CreatedAt string      `json:"createdAt"` // Consider parsing to time.Time if needed
		PostID    *int        `json:"postId"`    // Use pointer for optional field
		Stats     ImageStats  `json:"stats"`
		Meta      interface{} `json:"meta"` // Often unstructured JSON, use interface{}
		Username  string      `json:"username"`
	}

	ImageStats struct {
		CryCount     int `json:"cryCount"`
		LaughCount   int `json:"laughCount"`
		LikeCount    int `json:"likeCount"`
		HeartCount   int `json:"heartCount"`
		CommentCount int `json:"commentCount"`
	}

	ApiResponse struct { // Renamed from Response
		Items    []Model            `json:"items"`
		Metadata PaginationMetadata `json:"metadata"` // Added field for pagination
	}

	// Added struct for pagination metadata based on API docs
	PaginationMetadata struct {
		TotalItems  int    `json:"totalItems"`
		CurrentPage int    `json:"currentPage"`
		PageSize    int    `json:"pageSize"`
		TotalPages  int    `json:"totalPages"`
		NextPage    string `json:"nextPage"`
		PrevPage    string `json:"prevPage"`   // Added based on API docs
		NextCursor  string `json:"nextCursor"` // Added based on API docs (for images endpoint mainly)
	}

	// Internal file db entry for each model
	DatabaseEntry struct {
		ModelName    string       `json:"modelName"`
		ModelType    string       `json:"modelType"`
		Version      ModelVersion `json:"version"`
		File         File         `json:"file"`
		Timestamp    int64        `json:"timestamp"`
		Creator      Creator      `json:"creator"`
		Filename     string       `json:"filename"`
		Folder       string       `json:"folder"`
		Status       string       `json:"status"`
		ErrorDetails string       `json:"errorDetails,omitempty"`
	}

	// --- Start: /api/v1/images Endpoint Structures ---

	// ImageApiResponse represents the structure of the response from the /api/v1/images endpoint.
	ImageApiResponse struct {
		Items    []ImageApiItem   `json:"items"` // Renamed Image -> ImageApiItem to avoid conflict
		Metadata MetadataNextPage `json:"metadata"`
	}

	// ImageApiItem represents a single image item specifically from the /api/v1/images response.
	ImageApiItem struct {
		ID        int         `json:"id"`
		URL       string      `json:"url"`
		Hash      string      `json:"hash"` // Blurhash
		Width     int         `json:"width"`
		Height    int         `json:"height"`
		Nsfw      bool        `json:"nsfw"`      // Keep boolean for simplicity
		NsfwLevel string      `json:"nsfwLevel"` // None, Soft, Mature, X
		CreatedAt string      `json:"createdAt"`
		PostID    *int        `json:"postId"`
		Stats     ImageStats  `json:"stats"`
		Meta      interface{} `json:"meta"`
		Username  string      `json:"username"`
		BaseModel string      `json:"baseModel"`
	}

	// MetadataNextPage is used when the API returns metadata with a `nextPage` URL.
	MetadataNextPage struct {
		TotalItems   int    `json:"totalItems,omitempty"`
		CurrentPage  int    `json:"currentPage,omitempty"`
		PageSize     int    `json:"pageSize,omitempty"`
		NextCursor   string `json:"nextCursor,omitempty"`
		NextPage     string `json:"nextPage,omitempty"`
		PreviousPage string `json:"previousPage,omitempty"`
	}
	// --- End: /api/v1/images Endpoint Structures ---
)

// Database Status Constants
const (
	StatusPending    = "Pending"
	StatusDownloaded = "Downloaded"
	StatusError      = "Error"
)

// ConstructApiUrl builds the Civitai API URL from query parameters.
func ConstructApiUrl(params QueryParameters) string {
	base := "https://civitai.com/api/v1/models"
	values := url.Values{}

	// Add parameters if they have non-default values
	if params.Limit > 0 && params.Limit <= 100 { // Use API default if not set or invalid
		values.Set("limit", strconv.Itoa(params.Limit))
	} else {
		// Let the API use its default limit (usually 100)
	}

	if params.Page > 0 { // Page is only relevant for non-cursor pagination (less common now)
		// values.Set("page", strconv.Itoa(params.Page))
		// Generally, Cursor should be preferred over Page.
	}

	if params.Query != "" {
		values.Set("query", params.Query)
	}

	if params.Tag != "" {
		values.Set("tag", params.Tag)
	}

	if params.Username != "" {
		values.Set("username", params.Username)
	}

	for _, t := range params.Types {
		values.Add("types", t)
	}

	if params.Sort != "" {
		values.Set("sort", params.Sort)
	}

	if params.Period != "" {
		values.Set("period", params.Period)
	}

	if !params.AllowNoCredit { // Default is true, so only add if false
		values.Set("allowNoCredit", "false")
	}

	if !params.AllowDerivatives { // Default is true
		values.Set("allowDerivatives", "false")
	}

	if !params.AllowDifferentLicenses { // Default is true
		values.Set("allowDifferentLicense", "false") // API uses singular 'License'
	}

	if params.AllowCommercialUse != "Any" && params.AllowCommercialUse != "" { // Default is Any
		values.Set("allowCommercialUse", params.AllowCommercialUse)
	}

	// Only add nsfw param if true
	if params.Nsfw {
		values.Set("nsfw", "true")
	}

	for _, bm := range params.BaseModels {
		values.Add("baseModels", bm) // API uses camelCase
	}

	if params.Cursor != "" {
		values.Set("cursor", params.Cursor)
	}

	queryString := values.Encode()
	if queryString != "" {
		return base + "?" + queryString
	}
	return base
}
