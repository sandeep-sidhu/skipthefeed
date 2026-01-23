package main

import (
	"os"
	"path/filepath"
)

// Config holds all configuration for SkipTheFeed
type Config struct {
	// Data directory (contains databases, cookies, downloads)
	DataDir string

	// Server port
	Port string

	// Admin credentials for dashboard
	AdminUsername string
	AdminPassword string

	// Ollama AI configuration (optional)
	OllamaEnabled  bool
	OllamaURL      string
	OllamaModel    string
	OllamaUsername string
	OllamaPassword string

	// Binary paths (can be just the name if in PATH)
	YtDlpPath    string
	GalleryDlPath string

	// Instagram cookies file path
	InstagramCookiesFile string

	// Download directories
	InstagramDownloadDir string
	TwitterDownloadDir   string
	FacebookDownloadDir  string
	TempDownloadDir      string

	// Database paths
	WhatsAppDBPath   string
	DownloadsDBPath  string

	// Log file path
	LogFilePath string

	// Dashboard directory
	DashboardDir string
}

// LoadConfig loads configuration from environment variables with sensible defaults
func LoadConfig() *Config {
	dataDir := getEnv("SKIPTHEFEED_DATA_DIR", "./data")

	cfg := &Config{
		DataDir: dataDir,
		Port:    getEnv("SKIPTHEFEED_PORT", "3333"),

		// Admin credentials (password auto-generated if not set)
		AdminUsername: getEnv("SKIPTHEFEED_USERNAME", "admin"),
		AdminPassword: getEnv("SKIPTHEFEED_PASSWORD", ""),

		// Ollama AI (optional)
		OllamaEnabled:  getEnv("OLLAMA_ENABLED", "false") == "true",
		OllamaURL:      getEnv("OLLAMA_URL", ""),
		OllamaModel:    getEnv("OLLAMA_MODEL", "mistral:7b"),
		OllamaUsername: getEnv("OLLAMA_USERNAME", ""),
		OllamaPassword: getEnv("OLLAMA_PASSWORD", ""),

		// Binary paths - default to just the binary name (assumes in PATH)
		YtDlpPath:     getEnv("YTDLP_PATH", "yt-dlp"),
		GalleryDlPath: getEnv("GALLERYDL_PATH", "gallery-dl"),

		// Paths relative to data directory
		InstagramCookiesFile: filepath.Join(dataDir, "config", "instagram_cookies.txt"),
		InstagramDownloadDir: filepath.Join(dataDir, "downloads", "instagram"),
		TwitterDownloadDir:   filepath.Join(dataDir, "downloads", "twitter"),
		FacebookDownloadDir:  filepath.Join(dataDir, "downloads", "facebook"),
		TempDownloadDir:      filepath.Join(dataDir, "downloads", "temp"),
		WhatsAppDBPath:       filepath.Join(dataDir, "skipthefeed.db"),
		DownloadsDBPath:      filepath.Join(dataDir, "downloads.db"),
		LogFilePath:          filepath.Join(dataDir, "skipthefeed.log"),
		DashboardDir:         getEnv("SKIPTHEFEED_DASHBOARD_DIR", "./dashboard"),
	}

	return cfg
}

// EnsureDirectories creates all necessary directories
func (c *Config) EnsureDirectories() error {
	dirs := []string{
		c.DataDir,
		filepath.Join(c.DataDir, "config"),
		filepath.Join(c.DataDir, "downloads"),
		c.InstagramDownloadDir,
		c.TwitterDownloadDir,
		c.FacebookDownloadDir,
		c.TempDownloadDir,
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
