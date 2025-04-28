package cmd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus" // Import logrus for config loading message
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"go-civitai-download/internal/api"
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

// logLevel and logFormat are declared elsewhere (e.g., cmd_download_setup.go)
// var logLevel string
// var logFormat string

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

	// Add persistent flags for logging
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Logging level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "Logging format (text, json)")
	// NOTE: Viper binding for log level/format is not strictly necessary
	// as they are handled directly in initLogging() before Viper might be fully ready,
	// but we can add them for consistency if needed elsewhere.
	// viper.BindPFlag("loglevel", rootCmd.PersistentFlags().Lookup("log-level"))
	// viper.BindPFlag("logformat", rootCmd.PersistentFlags().Lookup("log-format"))

	// Add persistent flag for API logging
	rootCmd.PersistentFlags().BoolVar(&logApiFlag, "log-api", false, "Log API requests/responses to api.log (overrides config)")
	viper.BindPFlag("logapirequests", rootCmd.PersistentFlags().Lookup("log-api"))

	// Add persistent flag for save path
	rootCmd.PersistentFlags().StringVar(&savePathFlag, "save-path", "", "Directory to save models (overrides config)")
	viper.BindPFlag("savepath", rootCmd.PersistentFlags().Lookup("save-path"))

	// Add persistent flag for API delay
	// Default value 0 or negative means "use config or viper default"
	rootCmd.PersistentFlags().IntVar(&apiDelayFlag, "api-delay", -1, "Delay between API calls in ms (overrides config, -1 uses config default)")
	viper.BindPFlag("apidelayms", rootCmd.PersistentFlags().Lookup("api-delay"))

	// Add persistent flag for API timeout
	// Default value 0 or negative means "use config or viper default"
	rootCmd.PersistentFlags().IntVar(&apiTimeoutFlag, "api-timeout", -1, "Timeout for API HTTP client in seconds (overrides config, -1 uses config default)")
	viper.BindPFlag("apiclienttimeoutsec", rootCmd.PersistentFlags().Lookup("api-timeout"))

	// Set Viper defaults (these are applied only if not set in config file or by flag)
	viper.SetDefault("apidelayms", 200)         // Default polite delay
	viper.SetDefault("apiclienttimeoutsec", 60) // Default timeout

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	// rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

// loadGlobalConfig attempts to load the configuration and applies flag overrides.
// It also sets up the global HTTP transport based on logging settings.
func loadGlobalConfig(cmd *cobra.Command, args []string) error {
	// --- Configure Viper to read the config file ---
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)

		// Search config in home directory with name ".go-civitai-downloader" (without extension).
		viper.AddConfigPath(home)
		// Add current directory path
		viper.AddConfigPath(".")
		viper.SetConfigName("config") // Name of config file (without extension)
		viper.SetConfigType("toml")   // REQUIRED if the config file does not have the extension in the name
	}

	viper.AutomaticEnv() // read in environment variables that match

	// Normalize keys (e.g., from config like BaseModels to BASMODELS)
	// Might help resolve precedence issues with bound flags
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		log.Infof("Using configuration file: %s", viper.ConfigFileUsed())
		// Try merging again AFTER potential flag bindings? Seems redundant but maybe forces precedence.
		// if mergeErr := viper.MergeInConfig(); mergeErr != nil {
		// 	log.WithError(mergeErr).Warnf("Error merging config file after read: %s", viper.ConfigFileUsed())
		// }
	} else {
		// Handle errors reading the config file
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found; ignore error if desired
			log.Warnf("Config file not found. Using defaults and flags.")
		} else {
			// Config file was found but another error was produced
			log.WithError(err).Warnf("Error reading config file: %s", viper.ConfigFileUsed())
			// Don't make it fatal, let flags/defaults take over
		}
	}
	// --- Try merging config AFTER reading and AutomaticEnv ---
	if viper.ConfigFileUsed() != "" { // Only merge if a config file was actually used
		if err := viper.MergeInConfig(); err != nil {
			log.WithError(err).Warnf("Error explicitly merging config file: %s", viper.ConfigFileUsed())
		}
	}
	// --- End Viper config file reading ---

	// --- Unmarshal directly from the global viper instance AFTER ReadInConfig/Merge ---
	if err := viper.Unmarshal(&globalConfig); err != nil {
		log.WithError(err).Warnf("Error unmarshalling config into globalConfig struct: %v", err)
		// Don't make it fatal, allow commands to proceed with defaults/flags if possible.
	}

	// --- REMOVED: Manual merge of loaded config values into Viper ---

	log.Debug("Config loaded (or attempted). Viper will manage value precedence.")

	baseTransport := http.DefaultTransport

	// Check if API logging is enabled using Viper
	globalHttpTransport = baseTransport // Default to base transport
	log.Debugf("Initial globalHttpTransport type: %T", globalHttpTransport)

	if viper.GetBool("logapirequests") {
		log.Debug("API request logging enabled (via Viper), wrapping global HTTP transport.")
		// Define log file path
		logFilePath := "api.log"
		// Attempt to resolve relative to SavePath if possible, otherwise use current dir
		// Get SavePath using Viper
		savePath := viper.GetString("savepath")
		if savePath != "" {
			// Ensure SavePath exists (it might not if config loading failed partially)
			if _, statErr := os.Stat(savePath); statErr == nil {
				logFilePath = filepath.Join(savePath, logFilePath)
			} else {
				log.Warnf("SavePath '%s' (from Viper) not found, saving api.log to current directory.", savePath)
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
	// BUT: Rely on viper.Get*() for values potentially overridden by flags.
	return nil
}
