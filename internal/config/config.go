package config

import (
	"fmt"
	"go-civitai-download/internal/models" // Import models for the Config struct

	"github.com/BurntSushi/toml"
	log "github.com/sirupsen/logrus" // Use logrus
)

// LoadConfig reads the configuration from the specified path (defaulting to "config.toml")
// and populates the provided models.Config struct.
// It returns the loaded config and any error encountered.
// TODO: Make config path configurable.
func LoadConfig(configFilePath string) (models.Config, error) {
	if configFilePath == "" {
		configFilePath = "config.toml" // Default path
	}
	var cfg models.Config
	_, err := toml.DecodeFile(configFilePath, &cfg)
	if err != nil {
		// Return the error instead of logging fatal
		return models.Config{}, fmt.Errorf("error loading config file %s: %w", configFilePath, err)
	}

	// TODO: Add validation for required fields (e.g., SavePath, DatabasePath)
	if cfg.SavePath == "" {
		log.Warn("Warning: SavePath is not set in config.toml")
		// return models.Config{}, fmt.Errorf("SavePath is required in config file")
	}
	if cfg.DatabasePath == "" {
		log.Warn("Warning: DatabasePath is not set in config.toml")
		// return models.Config{}, fmt.Errorf("DatabasePath is required in config file")
	}

	log.Infof("Configuration loaded from %s", configFilePath)
	return cfg, nil
}
