package cmd

import (

	// Import bleve package directly

	"github.com/spf13/cobra"
)

// Variable shared by subcommands
var searchQuery string

// searchCmd represents the base search command when called without subcommands.
var searchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search the Bleve index for downloaded models or images",
	Long: `Provides subcommands to search the Bleve index created during downloads.
Use 'search models' or 'search images'.`,
	// No Run function, this is a parent command
}

func init() {
	rootCmd.AddCommand(searchCmd)

	// No flags defined here, they belong to subcommands (models, images)
}

// runSearch has been moved to search_logic.go as runSearchLogic
// func runSearch(cmd *cobra.Command, args []string) { ... }
