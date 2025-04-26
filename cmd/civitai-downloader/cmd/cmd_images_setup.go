package cmd

import (
	"github.com/spf13/viper"
)

func init() {
	// imagesCmd is defined in images.go
	rootCmd.AddCommand(imagesCmd)

	// --- Flags for Image Command ---
	imagesCmd.Flags().Int("limit", 100, "Max images per page (1-200).")
	imagesCmd.Flags().Int("post-id", 0, "Filter by Post ID.")
	imagesCmd.Flags().Int("model-id", 0, "Filter by Model ID.")
	imagesCmd.Flags().Int("model-version-id", 0, "Filter by Model Version ID (overrides model-id and post-id if set).")
	imagesCmd.Flags().StringP("username", "u", "", "Filter by username.")
	// Use string for nsfw flag to handle both boolean and enum values easily
	imagesCmd.Flags().String("nsfw", "", "Filter by NSFW level (None, Soft, Mature, X) or boolean (true/false). Empty means all.")
	imagesCmd.Flags().StringP("sort", "s", "Newest", "Sort order (Most Reactions, Most Comments, Newest).")
	imagesCmd.Flags().StringP("period", "p", "AllTime", "Time period for sorting (AllTime, Year, Month, Week, Day).")
	imagesCmd.Flags().Int("page", 1, "Starting page number (API defaults to 1).") // API uses page-based for images
	imagesCmd.Flags().Int("max-pages", 0, "Maximum number of API pages to fetch (0 for no limit)")
	imagesCmd.Flags().StringP("output-dir", "o", "", "Directory to save images (default: [SavePath]/images).")
	// Define a local variable for image command's concurrency flag
	var imageConcurrency int
	imagesCmd.Flags().IntVarP(&imageConcurrency, "concurrency", "c", 4, "Number of concurrent image downloads")
	// Add the save-metadata flag
	imagesCmd.Flags().Bool("metadata", false, "Save a .json metadata file alongside each downloaded image.")

	// Bind flags to Viper (optional)
	viper.BindPFlag("images.limit", imagesCmd.Flags().Lookup("limit"))
	viper.BindPFlag("images.postId", imagesCmd.Flags().Lookup("post-id"))
	viper.BindPFlag("images.modelId", imagesCmd.Flags().Lookup("model-id"))
	viper.BindPFlag("images.modelVersionId", imagesCmd.Flags().Lookup("model-version-id"))
	viper.BindPFlag("images.username", imagesCmd.Flags().Lookup("username"))
	viper.BindPFlag("images.nsfw", imagesCmd.Flags().Lookup("nsfw"))
	viper.BindPFlag("images.sort", imagesCmd.Flags().Lookup("sort"))
	viper.BindPFlag("images.period", imagesCmd.Flags().Lookup("period"))
	viper.BindPFlag("images.page", imagesCmd.Flags().Lookup("page"))
	viper.BindPFlag("images.max_pages", imagesCmd.Flags().Lookup("max-pages"))
	viper.BindPFlag("images.output_dir", imagesCmd.Flags().Lookup("output-dir"))
	viper.BindPFlag("images.concurrency", imagesCmd.Flags().Lookup("concurrency"))
	// Bind the new flag
	viper.BindPFlag("images.metadata", imagesCmd.Flags().Lookup("metadata"))
}
