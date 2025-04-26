package cmd

import (
	"fmt"

	"go-civitai-download/internal/models"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Variable to store concurrency level for flag parsing
var concurrencyLevel int

// Allowed values for API parameters
var allowedSortOrders = map[string]bool{
	"Highest Rated":   true,
	"Most Downloaded": true,
	"Newest":          true,
}

var allowedPeriods = map[string]bool{
	"AllTime": true,
	"Year":    true,
	"Month":   true,
	"Week":    true,
	"Day":     true,
}

// Variables defined in download.go that are used here
// var logLevel string // Declared in download.go
// var logFormat string // Declared in download.go

func init() {
	// Add downloadCmd to rootCmd
	// Note: downloadCmd itself is defined in download.go
	// This init() function needs to be called AFTER downloadCmd is defined.
	// Go execution order ensures this if both files are in the same package.
	rootCmd.AddCommand(downloadCmd)

	// Add persistent flags to rootCmd so they apply to all commands
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Logging level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "Logging format (text, json)")

	// Hook to configure logging before any command runs
	cobra.OnInitialize(initLogging)

	// Define flags specific to the download command
	downloadCmd.Flags().StringSliceP("type", "t", []string{}, "Filter by model type(s) (e.g., Checkpoint, LORA). Overrides config.")
	downloadCmd.Flags().StringSliceP("base-model", "b", []string{}, "Filter by base model(s) (e.g., \"SD 1.5\", SDXL). Overrides config.")
	downloadCmd.Flags().Bool("nsfw", false, "Include NSFW models (true/false). Overrides config if set.")
	downloadCmd.Flags().IntP("limit", "l", 100, "Max models per page (1-100).")
	downloadCmd.Flags().StringP("sort", "s", "Most Downloaded", "Sort order (Most Downloaded, Highest Rated, Newest).")
	downloadCmd.Flags().StringP("period", "p", "AllTime", "Time period for sorting (AllTime, Year, Month, Week, Day).")
	downloadCmd.Flags().Bool("primary-only", false, "Only download primary model files. Overrides config if set.")
	downloadCmd.Flags().StringP("query", "q", "", "Search query string.")
	downloadCmd.Flags().StringP("tag", "", "", "Filter by tag name.")
	downloadCmd.Flags().StringP("username", "u", "", "Filter by username.")
	downloadCmd.Flags().Bool("pruned", false, "Only download pruned Checkpoint models. Overrides config if set.")
	downloadCmd.Flags().Bool("fp16", false, "Only download fp16 Checkpoint models. Overrides config if set.")
	downloadCmd.Flags().IntVarP(&concurrencyLevel, "concurrency", "c", 4, "Number of concurrent downloads")
	downloadCmd.Flags().Bool("metadata", false, "Save a .json metadata file alongside each download. Overrides config.")
	downloadCmd.Flags().StringSlice("tags", []string{}, "Filter by tags (comma-separated)")
	downloadCmd.Flags().StringSlice("usernames", []string{}, "Filter by usernames (comma-separated)")
	downloadCmd.Flags().StringSliceP("model-types", "m", []string{}, "Filter by model types (e.g., Checkpoint, LORA, LoCon)")
	downloadCmd.Flags().Int("max-pages", 0, "Maximum number of API pages to fetch (0 for no limit)")
	downloadCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	downloadCmd.Flags().Bool("meta-only", false, "Only download and save .json metadata files, skip model download.")
	downloadCmd.Flags().Bool("model-info", false, "Save full model info JSON to '[SavePath]/model_info/[model.ID].json'.")
	downloadCmd.Flags().Int("model-version-id", 0, "Download a specific model version by ID, ignoring other filters like query, tags, etc.")
	downloadCmd.Flags().Int("model-id", 0, "Download versions for a specific model ID, ignoring general filters like query, tags, etc.")
	downloadCmd.Flags().Bool("version-images", false, "Download images associated with the specific downloaded model version.")
	downloadCmd.Flags().Bool("model-images", false, "When using --model-info, also download all images for all versions of the model.")
	downloadCmd.Flags().Bool("all-versions", false, "Download all versions of a model, not just the latest (overrides version selection).")

	// Bind flags to Viper
	viper.BindPFlag("download.tags", downloadCmd.Flags().Lookup("tags"))
	viper.BindPFlag("download.usernames", downloadCmd.Flags().Lookup("usernames"))
	viper.BindPFlag("download.model_types", downloadCmd.Flags().Lookup("model-types"))
	viper.BindPFlag("download.query", downloadCmd.Flags().Lookup("query"))
	viper.BindPFlag("download.sort", downloadCmd.Flags().Lookup("sort"))
	viper.BindPFlag("download.period", downloadCmd.Flags().Lookup("period"))
	viper.BindPFlag("download.limit", downloadCmd.Flags().Lookup("limit"))
	viper.BindPFlag("download.max_pages", downloadCmd.Flags().Lookup("max-pages"))
	viper.BindPFlag("download.concurrency", downloadCmd.Flags().Lookup("concurrency"))
	viper.BindPFlag("download.metadata", downloadCmd.Flags().Lookup("metadata"))
	viper.BindPFlag("download.primary-only", downloadCmd.Flags().Lookup("primary-only"))
	viper.BindPFlag("download.pruned", downloadCmd.Flags().Lookup("pruned"))
	viper.BindPFlag("download.fp16", downloadCmd.Flags().Lookup("fp16"))
	viper.BindPFlag("download.yes", downloadCmd.Flags().Lookup("yes"))
	viper.BindPFlag("download.meta_only", downloadCmd.Flags().Lookup("meta-only"))
	viper.BindPFlag("download.model_info", downloadCmd.Flags().Lookup("model-info"))
	viper.BindPFlag("download.model_version_id", downloadCmd.Flags().Lookup("model-version-id"))
	viper.BindPFlag("download.model_id", downloadCmd.Flags().Lookup("model-id"))
	viper.BindPFlag("download.version_images", downloadCmd.Flags().Lookup("version-images"))
	viper.BindPFlag("download.model_images", downloadCmd.Flags().Lookup("model-images"))
	viper.BindPFlag("download.all_versions", downloadCmd.Flags().Lookup("all-versions"))
}

// initLogging configures logrus based on persistent flags
func initLogging() {
	level, err := log.ParseLevel(logLevel)
	if err != nil {
		log.WithError(err).Warnf("Invalid log level '%s', using default 'info'", logLevel)
		level = log.InfoLevel
	}
	log.SetLevel(level)

	switch logFormat {
	case "json":
		log.SetFormatter(&log.JSONFormatter{})
	case "text":
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	default:
		log.Warnf("Invalid log format '%s', using default 'text'", logFormat)
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	}

	log.Infof("Logging configured: Level=%s, Format=%s", log.GetLevel(), logFormat)
}

// setupQueryParams initializes the query parameters based on global config and flags.
func setupQueryParams(cfg *models.Config, cmd *cobra.Command) models.QueryParameters {
	params := models.QueryParameters{
		Limit:                  cfg.Limit, // Use cfg.Limit as default
		Page:                   1,         // Start at page 1
		Query:                  cfg.Query, // Use config value
		Tag:                    "",        // Tag/Username likely come from specific flags, not general config
		Username:               "",
		Types:                  cfg.ModelTypes,  // Use renamed field
		Sort:                   cfg.Sort,        // Use Sort from config
		Period:                 cfg.Period,      // Use Period from config
		Rating:                 0,               // Optional: Filter by rating
		Favorites:              false,           // Optional: Filter by favorites
		Hidden:                 false,           // Optional: Filter by hidden status
		PrimaryFileOnly:        cfg.PrimaryOnly, // Use renamed field
		AllowNoCredit:          true,            // Default based on typical usage
		AllowDerivatives:       true,            // Default based on typical usage
		AllowDifferentLicenses: true,            // Default based on typical usage
		AllowCommercialUse:     "Any",           // Default based on typical usage
		Nsfw:                   cfg.Nsfw,        // Use renamed field
		BaseModels:             cfg.BaseModels,  // Use BaseModels from config
	}

	// Apply Tag/Username from potentially specific config fields if needed
	// if len(cfg.Tags) > 0 { params.Tag = strings.Join(cfg.Tags, ",") } // Example if Tag API takes one
	// if len(cfg.Usernames) > 0 { params.Username = strings.Join(cfg.Usernames, ",") } // Example if Username API takes one
	// --> NOTE: The API seems to take single tag/username, so flags are better here.
	// Keep params.Tag and params.Username initialized to "" and let flags override.

	// Validate initial Sort from config
	if _, ok := allowedSortOrders[params.Sort]; !ok && params.Sort != "" { // Check if set and not allowed
		log.Warnf("Invalid Sort value '%s' in config, using default 'Most Downloaded'", params.Sort)
		params.Sort = "Most Downloaded"
	} else if params.Sort == "" { // Assign default if empty
		params.Sort = "Most Downloaded"
	}

	// Validate initial Period from config
	if _, ok := allowedPeriods[params.Period]; !ok && params.Period != "" { // Check if set and not allowed
		log.Warnf("Invalid Period value '%s' in config, using default 'AllTime'", params.Period)
		params.Period = "AllTime"
	} else if params.Period == "" { // Assign default if empty
		params.Period = "AllTime"
	}

	// Validate initial Limit from config
	if cfg.Limit <= 0 || cfg.Limit > 100 {
		if cfg.Limit != 0 { // Don't warn if it was just omitted (zero value)
			log.Warnf("Invalid Limit value '%d' in config, using default 100", cfg.Limit)
		}
		params.Limit = 100
	} else {
		params.Limit = cfg.Limit // Use valid config limit
	}

	// Override QueryParameter fields with flags if set
	if cmd.Flags().Changed("type") { // This flag might be deprecated/renamed if ModelTypes is preferred
		types, _ := cmd.Flags().GetStringSlice("type")
		log.WithField("types", types).Debug("Overriding ModelTypes with --type flag value")
		params.Types = types
	}
	if cmd.Flags().Changed("model-types") { // Use the newer flag name
		types, _ := cmd.Flags().GetStringSlice("model-types")
		log.WithField("modelTypes", types).Debug("Overriding ModelTypes with --model-types flag value")
		params.Types = types
	}
	if cmd.Flags().Changed("base-model") {
		baseModels, _ := cmd.Flags().GetStringSlice("base-model")
		log.WithField("baseModels", baseModels).Debug("Overriding BaseModels with flag value")
		params.BaseModels = baseModels // Note: API uses 'baseModels', mapping handled in client
	}
	if cmd.Flags().Changed("nsfw") {
		nsfw, _ := cmd.Flags().GetBool("nsfw")
		log.WithField("nsfw", nsfw).Debug("Overriding Nsfw with flag value")
		params.Nsfw = nsfw
	}
	if cmd.Flags().Changed("limit") {
		limit, _ := cmd.Flags().GetInt("limit")
		if limit > 0 && limit <= 100 {
			log.Debugf("Overriding Limit with flag value: %d", limit)
			params.Limit = limit
		} else {
			log.Warnf("Invalid limit value '%d' from flag, ignoring flag and using value: %d", limit, params.Limit)
		}
	}
	if cmd.Flags().Changed("sort") {
		sort, _ := cmd.Flags().GetString("sort")
		if _, ok := allowedSortOrders[sort]; ok {
			log.Debugf("Overriding Sort with flag value: %s", sort)
			params.Sort = sort
		} else {
			log.Warnf("Invalid sort value '%s' from flag, ignoring flag and using value: %s", sort, params.Sort)
		}
	}
	if cmd.Flags().Changed("period") {
		period, _ := cmd.Flags().GetString("period")
		if _, ok := allowedPeriods[period]; ok {
			log.Debugf("Overriding Period with flag value: %s", period)
			params.Period = period
		} else {
			log.Warnf("Invalid period value '%s' from flag, ignoring flag and using value: %s", period, params.Period)
		}
	}
	if cmd.Flags().Changed("primary-only") {
		primaryOnly, _ := cmd.Flags().GetBool("primary-only")
		log.WithField("primaryOnly", primaryOnly).Debug("Overriding PrimaryFileOnly with flag value")
		params.PrimaryFileOnly = primaryOnly
	}

	if cmd.Flags().Changed("query") {
		query, _ := cmd.Flags().GetString("query")
		params.Query = query
		log.Debugf("Setting Query from flag: %s", query)
	}
	if cmd.Flags().Changed("tag") {
		tag, _ := cmd.Flags().GetString("tag")
		params.Tag = tag
		log.Debugf("Setting Tag from flag: %s", tag)
	}
	if cmd.Flags().Changed("username") {
		username, _ := cmd.Flags().GetString("username")
		params.Username = username
		log.Debugf("Setting Username from flag: %s", username)
	}

	log.WithField("params", fmt.Sprintf("%+v", params)).Debug("Final query parameters set")
	return params
}
