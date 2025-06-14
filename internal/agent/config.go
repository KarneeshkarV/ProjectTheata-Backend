package agent

import (
	"os"
)

// Config holds the agent configuration
type Config struct {
	BaseURL        string
	AppName        string
	DefaultUserID  string
	DefaultSession string
}

// DefaultConfig returns the default configuration
func DefaultConfig() Config {
	return Config{
		BaseURL:        ADKAgentBaseURL,
		AppName:        ADKAgentAppName,
		DefaultUserID:  DefaultADKUserID,
		DefaultSession: DefaultADKSessionID,
	}
}

// LoadConfigFromEnv loads configuration from environment variables if available
func LoadConfigFromEnv() Config {
	config := DefaultConfig()
	
	if baseURL := os.Getenv("ADK_AGENT_BASE_URL"); baseURL != "" {
		config.BaseURL = baseURL
	}
	
	if appName := os.Getenv("ADK_AGENT_APP_NAME"); appName != "" {
		config.AppName = appName
	}
	
	if userID := os.Getenv("ADK_DEFAULT_USER_ID"); userID != "" {
		config.DefaultUserID = userID
	}
	
	if sessionID := os.Getenv("ADK_DEFAULT_SESSION_ID"); sessionID != "" {
		config.DefaultSession = sessionID
	}
	
	return config
}
