package index

import (
	"log"
	"os"
	"time"

	"github.com/blevesearch/bleve/v2"
)

const defaultIndexPath = "civitai.bleve"

// Item represents a generic item to be indexed.
// We might need more specific structs later for models, images, etc.
// By default, all fields defined here are indexed and searchable using their
// lowercase JSON tag names (e.g., query '+creatorName:someuser' or '+tags:tagname').
type Item struct {
	ID            string   `json:"id"`                      // Unique ID (e.g., v_<version_id>, img_<image_id>)
	Type          string   `json:"type"`                    // Type of item (e.g., "model_file", "image")
	Name          string   `json:"name"`                    // Name of the item (file name, image name if available)
	Description   string   `json:"description"`             // Description or other text content
	FilePath      string   `json:"filePath"`                // Path where the item is downloaded
	DirectoryPath string   `json:"directoryPath,omitempty"` // Directory containing the file
	BaseModelPath string   `json:"baseModelPath,omitempty"` // Path up to the base model slug
	ModelPath     string   `json:"modelPath,omitempty"`     // Path up to the model name slug
	ModelName     string   `json:"modelName,omitempty"`     // Name of the parent model (for model files/images)
	VersionName   string   `json:"versionName,omitempty"`   // Name of the model version (for model files)
	BaseModel     string   `json:"baseModel,omitempty"`     // Base model (e.g., SDXL 1.0)
	CreatorName   string   `json:"creatorName,omitempty"`   // Username of the creator
	Tags          []string `json:"tags,omitempty"`          // Associated tags (if available)
	Prompt        string   `json:"prompt,omitempty"`        // Image generation prompt (for images)
	NsfwLevel     string   `json:"nsfwLevel,omitempty"`     // NSFW Level (for images)

	// New Fields
	PublishedAt          time.Time `json:"publishedAt,omitempty"`          // Version publication timestamp
	VersionDownloadCount float64   `json:"versionDownloadCount,omitempty"` // Version download count
	VersionRating        float64   `json:"versionRating,omitempty"`        // Version rating
	VersionRatingCount   float64   `json:"versionRatingCount,omitempty"`   // Version rating count
	FileSizeKB           float64   `json:"fileSizeKB,omitempty"`           // File size in KB
	FileFormat           string    `json:"fileFormat,omitempty"`           // File format (e.g., safetensor)
	FilePrecision        string    `json:"filePrecision,omitempty"`        // File precision (e.g., fp16)
	FileSizeType         string    `json:"fileSizeType,omitempty"`         // File size type (e.g., pruned)

	// Torrent Information (populated by the 'torrent' command)
	TorrentPath string `json:"torrentPath,omitempty"` // Path to the downloaded .torrent file
	MagnetLink  string `json:"magnetLink,omitempty"`  // Magnet link for the torrent
}

// OpenOrCreateIndex opens an existing Bleve index or creates a new one if it doesn't exist.
func OpenOrCreateIndex(indexPath string) (bleve.Index, error) {
	if indexPath == "" {
		indexPath = defaultIndexPath
	}

	index, err := bleve.Open(indexPath)
	if err == bleve.ErrorIndexPathDoesNotExist {
		log.Printf("Creating new index at: %s", indexPath)
		mapping := bleve.NewIndexMapping()
		// Customize mapping here if needed (e.g., for specific fields)
		index, err = bleve.New(indexPath, mapping)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err // Other error opening index
	} else {
		log.Printf("Opened existing index at: %s", indexPath)
	}
	return index, nil
}

// IndexItem adds or updates an item in the Bleve index.
func IndexItem(index bleve.Index, item Item) error {
	return index.Index(item.ID, item)
}

// SearchIndex performs a search query against the index.
func SearchIndex(index bleve.Index, query string) (*bleve.SearchResult, error) {
	searchQuery := bleve.NewQueryStringQuery(query)
	searchRequest := bleve.NewSearchRequest(searchQuery)
	searchRequest.Fields = []string{"*"} // Request all stored fields
	searchResults, err := index.Search(searchRequest)
	if err != nil {
		return nil, err
	}
	return searchResults, nil
}

// DeleteIndex removes the index directory. Use with caution!
func DeleteIndex(indexPath string) error {
	if indexPath == "" {
		indexPath = defaultIndexPath
	}
	log.Printf("Attempting to delete index at: %s", indexPath)
	return os.RemoveAll(indexPath)
}
