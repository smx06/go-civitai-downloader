package cmd

import (
	"fmt"

	index "go-civitai-download/index"

	"github.com/blevesearch/bleve/v2" // Import bleve package directly
	log "github.com/sirupsen/logrus"
	// Note: No cobra import needed here as flags are handled by subcommands
)

// runSearchLogic executes the search against a specific index path.
// It's called by the subcommand Run functions.
func runSearchLogic(indexPath string, query string) {
	// Logging should already be initialized by the time this is called
	log.Debugf("runSearchLogic called with indexPath: %s, query: %s", indexPath, query)

	if query == "" {
		// This check should ideally happen in the subcommand before calling,
		// but double-checking here.
		log.Error("Search query cannot be empty.")
		return
	}

	if indexPath == "" {
		log.Error("Index path cannot be empty.")
		return
	}

	log.Infof("Opening Bleve index at: %s", indexPath)
	// Use Open instead of OpenOrCreateIndex to avoid creating index during search
	bleveIndex, err := bleve.Open(indexPath)
	if err != nil {
		if err == bleve.ErrorIndexPathDoesNotExist { // Check against bleve's error constant
			log.Errorf("Bleve index not found at %s. Run the corresponding download command first to create it.", indexPath)
		} else {
			log.Errorf("Failed to open Bleve index at %s: %v", indexPath, err)
		}
		return // Return instead of Fatal to allow potential multi-index search later
	}
	defer func() {
		log.Debug("Closing Bleve index.")
		if err := bleveIndex.Close(); err != nil {
			log.Errorf("Error closing Bleve index: %v", err)
		}
	}()

	log.Infof("Performing search with query: %s", query)

	searchResults, err := index.SearchIndex(bleveIndex, query)
	if err != nil {
		log.Errorf("Error performing search: %v", err)
		return
	}

	log.Infof("Search finished. Hits: %d, Total: %d, Took: %s",
		len(searchResults.Hits),
		searchResults.Total,
		searchResults.Took)

	if searchResults.Total > 0 {
		fmt.Println("--- Search Results ---")
		for i, hit := range searchResults.Hits {
			fmt.Printf("[%d] ID: %s (Score: %.2f)\n", i+1, hit.ID, hit.Score)
			// Print requested fields (all fields are requested by SearchIndex)
			for field, value := range hit.Fields {
				fmt.Printf("  %s: %v\n", field, value)
			}
			fmt.Println("---")
		}
	} else {
		fmt.Println("No results found matching your query.")
	}
}
