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

// REMOVED init() function to avoid flag redefinition.
// Flag definitions and bindings are now consolidated in download.go's init().

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

// setupQueryParams initializes the query parameters using Viper for flag/config precedence.
func setupQueryParams(cfg *models.Config, cmd *cobra.Command) models.QueryParameters {
	// Viper keys should match the keys used in viper.BindPFlag in init()

	// Use viper.Get* for values that can be set by flags
	limit := viper.GetInt("limit") // Viper key from download.go init
	if limit <= 0 || limit > 100 {
		if limit != 0 { // Don't warn if just using default
			log.Warnf("Invalid Limit value '%d' from flag/config, using default 100", limit)
		}
		limit = 100 // API default/max
	}

	sort := viper.GetString("sort") // Viper key from download.go init
	if _, ok := allowedSortOrders[sort]; !ok && sort != "" {
		log.Warnf("Invalid Sort value '%s' from flag/config, using default 'Most Downloaded'", sort)
		sort = "Most Downloaded"
	} else if sort == "" {
		sort = "Most Downloaded"
	}

	period := viper.GetString("period") // Viper key from download.go init
	if _, ok := allowedPeriods[period]; !ok && period != "" {
		log.Warnf("Invalid Period value '%s' from flag/config, using default 'AllTime'", period)
		period = "AllTime"
	} else if period == "" {
		period = "AllTime"
	}

	params := models.QueryParameters{
		Limit:                  limit,
		Page:                   1,                                  // Start at page 1
		Query:                  viper.GetString("query"),           // Viper key from download.go init
		Tag:                    viper.GetString("tag"),             // Viper key from download.go init - Assuming API takes single tag
		Username:               viper.GetString("username"),        // Viper key from download.go init - Assuming API takes single username
		Types:                  viper.GetStringSlice("modeltypes"), // Viper key from download.go init
		Sort:                   sort,
		Period:                 period,
		Rating:                 0,                                  // Not configured via flag/config currently
		Favorites:              false,                              // Not configured via flag/config currently
		Hidden:                 false,                              // Not configured via flag/config currently
		PrimaryFileOnly:        viper.GetBool("primaryonly"),       // Viper key from download.go init
		AllowNoCredit:          true,                               // Default based on typical usage
		AllowDerivatives:       true,                               // Default based on typical usage
		AllowDifferentLicenses: true,                               // Default based on typical usage
		AllowCommercialUse:     "Any",                              // Default based on typical usage
		Nsfw:                   viper.GetBool("nsfw"),              // Viper key from download.go init
		BaseModels:             viper.GetStringSlice("basemodels"), // Viper key from download.go init
	}

	// Removed manual flag override checks - Viper handles precedence.

	log.WithField("params", fmt.Sprintf("%+v", params)).Debug("Final query parameters set")
	return params
}
