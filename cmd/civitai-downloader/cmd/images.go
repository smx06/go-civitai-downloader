package cmd

import (
	"github.com/spf13/cobra"
)

// imagesCmd represents the images command
var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Download images based on various criteria (model, user, etc.)",
	Long: `Downloads images from Civitai based on filters like model ID, model version ID,
or username. Allows specifying limits, sorting, and NSFW preferences.

Examples:
  # Download latest 20 images for model ID 123
  civitai-downloader images --model-id 123 --limit 20

  # Download all SFW images for model version ID 456, sorted by most reactions
  civitai-downloader images --model-version-id 456 --sort "Most Reactions" --nsfw=None

  # Download the 50 most popular images of all time from user 'exampleUser'
  civitai-downloader images --username exampleUser --limit 50 --period AllTime --sort MostPopular`,
	Run: runImages,
}
