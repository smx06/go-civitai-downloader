package cmd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus" // Import logrus for config loading message
	"github.com/spf13/cobra"

	"go-civitai-download/internal/api"
	"go-civitai-download/internal/config"
	"go-civitai-download/internal/models"
)

// cfgFile holds the path to the config file specified by the user
var cfgFile string

// logApiFlag holds the value of the --log-api flag
var logApiFlag bool

// savePathFlag holds the value of the --save-path flag
var savePathFlag string

// apiDelayFlag holds the value of the --api-delay flag
var apiDelayFlag int

// apiTimeoutFlag holds the value of the --api-timeout flag
var apiTimeoutFlag int

// globalConfig holds the loaded configuration
var globalConfig models.Config

// globalHttpTransport holds the globally configured HTTP transport (base or logging-wrapped)
var globalHttpTransport http.RoundTripper

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "civitai-downloader",
	Short: "A tool to download models from Civitai",
	Long: `Civitai Downloader allows you to fetch and manage models 
from Civitai.com based on specified criteria.`,
	PersistentPreRunE: loadGlobalConfig, // Load config before any command runs
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	// Defer closing the API logging transport if it was initialized
	defer func() {
		if loggingTransport, ok := globalHttpTransport.(*api.LoggingTransport); ok && loggingTransport != nil {
			log.Debug("Closing API logging transport file.")
			log.Debug("Attempting to call loggingTransport.Close()...")
			if err := loggingTransport.Close(); err != nil {
				log.WithError(err).Error("Error closing API log file")
			}
			log.Debug("Returned from loggingTransport.Close().")
		} else {
			log.Debugf("Global HTTP transport is not the logging transport (type: %T), skipping close.", globalHttpTransport)
		}
	}()

	// cobra.OnInitialize(initConfig) // We use PersistentPreRunE now
	err := rootCmd.Execute()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing command: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	// Add persistent flags that apply to all commands
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "config.toml", "Configuration file path")
	// Add persistent flag for API logging
	rootCmd.PersistentFlags().BoolVar(&logApiFlag, "log-api", false, "Log API requests/responses to api.log (overrides config)")
	// Add persistent flag for save path
	rootCmd.PersistentFlags().StringVar(&savePathFlag, "save-path", "", "Directory to save models (overrides config)")
	// Add persistent flag for API delay
	rootCmd.PersistentFlags().IntVar(&apiDelayFlag, "api-delay", -1, "Delay between API calls in ms (overrides config, -1 uses config default)")
	// Add persistent flag for API timeout
	rootCmd.PersistentFlags().IntVar(&apiTimeoutFlag, "api-timeout", -1, "Timeout for API HTTP client in seconds (overrides config, -1 uses config default)")

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	// rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

// loadGlobalConfig attempts to load the configuration and applies flag overrides.
// It also sets up the global HTTP transport based on logging settings.
func loadGlobalConfig(cmd *cobra.Command, args []string) error {
	var err error
	globalConfig, err = config.LoadConfig(cfgFile)
	if err != nil {
		// Log a warning but don't make it fatal here,
		// as some commands might not strictly require a config (though most will).
		// Commands should check the fields they need from globalConfig.
		log.WithError(err).Warnf("Failed to load configuration from %s", cfgFile)
		// We return nil here to allow commands to proceed and potentially fail later
		// if they require specific config values.
		// return fmt.Errorf("failed to load config: %w", err)
	}

	// Override LogApiRequests if flag was used
	if cmd.Flags().Changed("log-api") {
		globalConfig.LogApiRequests = logApiFlag
		log.Debugf("Overriding LogApiRequests based on --log-api flag: %t", logApiFlag)
	}

	// Override SavePath if flag was used
	if cmd.Flags().Changed("save-path") {
		if savePathFlag != "" { // Ensure the flag value is not empty
			globalConfig.SavePath = savePathFlag
			log.Debugf("Overriding SavePath based on --save-path flag: %s", savePathFlag)
		} else {
			log.Warn("--save-path flag provided but value is empty, ignoring.")
		}
	}

	// Override ApiDelayMs if flag was used and valid
	if cmd.Flags().Changed("api-delay") {
		if apiDelayFlag >= 0 { // Allow 0 delay if specified
			globalConfig.ApiDelayMs = apiDelayFlag
			log.Debugf("Overriding ApiDelayMs based on --api-delay flag: %d ms", apiDelayFlag)
		} else {
			log.Warnf("--api-delay flag provided with invalid value %d, using config value: %d ms", apiDelayFlag, globalConfig.ApiDelayMs)
		}
	}

	// Ensure default delay if not set
	if globalConfig.ApiDelayMs < 0 {
		log.Debugf("ApiDelayMs not set or invalid in config/flags, defaulting to 200ms")
		globalConfig.ApiDelayMs = 200 // Default polite delay
	}

	// Override ApiClientTimeoutSec if flag was used and valid
	if cmd.Flags().Changed("api-timeout") {
		if apiTimeoutFlag > 0 { // Timeout must be positive
			globalConfig.ApiClientTimeoutSec = apiTimeoutFlag
			log.Debugf("Overriding ApiClientTimeoutSec based on --api-timeout flag: %d sec", apiTimeoutFlag)
		} else {
			log.Warnf("--api-timeout flag provided with invalid value %d, using config value: %d sec", apiTimeoutFlag, globalConfig.ApiClientTimeoutSec)
		}
	}

	// Ensure default timeout if not set or invalid
	if globalConfig.ApiClientTimeoutSec <= 0 {
		log.Debugf("ApiClientTimeoutSec not set or invalid in config/flags, defaulting to 60s")
		globalConfig.ApiClientTimeoutSec = 60 // Default timeout
	}

	// Override config SaveMetadata setting if the flag was explicitly used
	if cmd.Flags().Changed("save-metadata") {
		// Note: Need to read the flag value here if we want to bind it
		// For bool flags, just knowing it Changed might be enough if default is false
		// but let's read it for clarity
		metaFlag, _ := cmd.Flags().GetBool("save-metadata")
		globalConfig.SaveMetadata = metaFlag
		log.Debugf("Overriding SaveMetadata based on --save-metadata flag: %t", metaFlag)
	}

	// Add log to check final config value
	log.Debugf("Final LogApiRequests value after config load and flag check: %t", globalConfig.LogApiRequests)

	// --- Setup Global HTTP Transport ---
	// Create the base transport (can be simple default or customized)
	baseTransport := http.DefaultTransport

	// Check if API logging is enabled
	globalHttpTransport = baseTransport // Default to base transport
	log.Debugf("Initial globalHttpTransport type: %T", globalHttpTransport)
	if globalConfig.LogApiRequests {
		log.Debug("API request logging enabled, wrapping global HTTP transport.")
		// Define log file path
		logFilePath := "api.log"
		// Attempt to resolve relative to SavePath if possible, otherwise use current dir
		if globalConfig.SavePath != "" {
			// Ensure SavePath exists (it might not if config loading failed partially)
			if _, statErr := os.Stat(globalConfig.SavePath); statErr == nil {
				logFilePath = filepath.Join(globalConfig.SavePath, logFilePath)
			} else {
				log.Warnf("SavePath '%s' not found, saving api.log to current directory.", globalConfig.SavePath)
			}
		}
		log.Infof("API logging to file: %s", logFilePath)

		// Initialize the logging transport
		loggingTransport, err := api.NewLoggingTransport(baseTransport, logFilePath)
		if err != nil {
			log.WithError(err).Error("Failed to initialize API logging transport, logging disabled.")
			// Keep globalHttpTransport as baseTransport
		} else {
			globalHttpTransport = loggingTransport // Use the wrapped transport
		}
	}
	// --- End Setup Global HTTP Transport ---

	// If successful or partially successful, globalConfig is populated for use by commands.
	return nil
}
