package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	WamClient   *whatsmeow.Client
	log         waLog.Logger
	downloadsDB *sql.DB

	// Logging infrastructure
	logFile      *os.File
	logClients   = make(map[chan string]bool)
	logClientsMu sync.RWMutex

	// Global config
	cfg *Config

	// QR code for web display
	currentQRCode   string
	qrCodeMu        sync.RWMutex
	isAuthenticated bool

	// Auth state
	authConfig     *AuthConfig
	authConfigMu   sync.RWMutex
	activeSessions = make(map[string]time.Time) // session token -> expiry
	sessionsMu     sync.RWMutex
)

// AuthConfig holds authentication configuration
type AuthConfig struct {
	SetupDone    bool   `json:"setupDone"`
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
	Password     string `json:"password"` // Store plain password for display on startup
}

// BotSettings holds configurable bot settings
type BotSettings struct {
	AutoDownloadEnabled   bool   `json:"autoDownloadEnabled"`
	WelcomeMessageEnabled bool   `json:"welcomeMessageEnabled"`
	WelcomeMessageText    string `json:"welcomeMessageText"`
}

var (
	botSettings   *BotSettings
	botSettingsMu sync.RWMutex
)

const defaultWelcomeMessage = `✨ *SkipTheFeed* replaced that link with the actual video!

Watch just the video, skip the feed. No more getting sucked into endless scrolling.`

// Phonetic words for generating memorable passwords
var phoneticWords = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
	"golf", "hotel", "india", "juliet", "kilo", "lima",
	"mike", "november", "oscar", "papa", "quebec", "romeo",
	"sierra", "tango", "uniform", "victor", "whiskey", "xray",
	"yankee", "zulu", "red", "blue", "green", "orange",
	"purple", "silver", "golden", "crystal", "thunder", "falcon",
}

// generatePassword creates a memorable phonetic password
func generatePassword() string {
	words := make([]string, 3)
	for i := 0; i < 3; i++ {
		idx := make([]byte, 1)
		rand.Read(idx)
		words[i] = phoneticWords[int(idx[0])%len(phoneticWords)]
	}
	return strings.Join(words, "-")
}

// generateToken creates a random token for sessions
func generateToken(length int) string {
	bytes := make([]byte, length)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// hashPassword creates a SHA256 hash of the password
func hashPassword(password string) string {
	hash := sha256.Sum256([]byte(password))
	return hex.EncodeToString(hash[:])
}

// loadAuthConfig loads auth configuration from environment variables or generates defaults
func loadAuthConfig() error {
	username := cfg.AdminUsername
	password := cfg.AdminPassword

	// Generate password if not set via environment variable
	passwordFromEnv := password != ""
	if !passwordFromEnv {
		password = generatePassword()
	}

	authConfig = &AuthConfig{
		SetupDone:    true,
		Username:     username,
		PasswordHash: hashPassword(password),
		Password:     password,
	}

	// Display credentials on startup
	fmt.Println("")
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    PORTAL CREDENTIALS                      ║")
	fmt.Println("╠════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Username: %-47s║\n", authConfig.Username)
	fmt.Printf("║  Password: %-47s║\n", authConfig.Password)
	if passwordFromEnv {
		fmt.Println("║  (configured via environment variable)                     ║")
	} else {
		fmt.Println("║  (auto-generated - set SKIPTHEFEED_PASSWORD to change)     ║")
	}
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println("")

	return nil
}

// loadBotSettings loads or creates bot settings
func loadBotSettings() error {
	settingsPath := filepath.Join(cfg.DataDir, "config", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			// First time - create default settings
			botSettings = &BotSettings{
				AutoDownloadEnabled:   true,
				WelcomeMessageEnabled: true,
				WelcomeMessageText:    defaultWelcomeMessage,
			}
			return saveBotSettings()
		}
		return err
	}

	botSettings = &BotSettings{}
	if err := json.Unmarshal(data, botSettings); err != nil {
		return err
	}

	// Ensure welcome message has a default if empty
	if botSettings.WelcomeMessageText == "" {
		botSettings.WelcomeMessageText = defaultWelcomeMessage
	}

	return nil
}

// saveBotSettings saves bot settings
func saveBotSettings() error {
	settingsPath := filepath.Join(cfg.DataDir, "config", "settings.json")

	botSettingsMu.RLock()
	data, err := json.MarshalIndent(botSettings, "", "  ")
	botSettingsMu.RUnlock()

	if err != nil {
		return err
	}

	return os.WriteFile(settingsPath, data, 0644)
}

// createSession creates a new session and returns the token
func createSession() string {
	token := generateToken(32)
	sessionsMu.Lock()
	activeSessions[token] = time.Now().Add(24 * time.Hour) // 24 hour session
	sessionsMu.Unlock()
	return token
}

// validateSession checks if a session token is valid
func validateSession(token string) bool {
	sessionsMu.RLock()
	expiry, exists := activeSessions[token]
	sessionsMu.RUnlock()

	if !exists {
		return false
	}

	if time.Now().After(expiry) {
		// Session expired, remove it
		sessionsMu.Lock()
		delete(activeSessions, token)
		sessionsMu.Unlock()
		return false
	}

	return true
}

// deleteSession removes a session
func deleteSession(token string) {
	sessionsMu.Lock()
	delete(activeSessions, token)
	sessionsMu.Unlock()
}

// Instagram URL pattern - matches posts, reels, and stories
var instagramURLRegex = regexp.MustCompile(`https?://(?:www\.)?instagram\.com/(?:p|reel|stories)/([A-Za-z0-9_-]+)/?`)

// Twitter/X URL pattern - matches tweets and video posts
var twitterURLRegex = regexp.MustCompile(`https?://(?:www\.)?(?:twitter\.com|x\.com)/\w+/status/(\d+)/?`)

// Facebook URL pattern - matches videos, reels, watch, and share links
var facebookURLRegex = regexp.MustCompile(`https?://(?:www\.|m\.)?(?:facebook\.com|fb\.watch)/(?:watch/?\?v=\d+|reel/\d+|[^/]+/videos/\d+|share/v/[A-Za-z0-9]+|[^/]+/posts/[^/\s]+)/?`)

// YouTube URL pattern - matches shorts, watch, and youtu.be links
var youtubeURLRegex = regexp.MustCompile(`https?://(?:www\.)?(?:youtube\.com/(?:shorts/|watch\?v=)|youtu\.be/)([A-Za-z0-9_-]+)`)

// Maximum file size for YouTube downloads (10MB)
const maxYouTubeFileSize = 10 * 1024 * 1024


// initLogging initializes file logging
func initLogging() error {
	var err error
	logFile, err = os.OpenFile(cfg.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	return nil
}

// logBoth logs a message to stdout, file, and broadcasts to SSE clients
func logBoth(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("[%s] [%s] %s", timestamp, level, msg)

	// Print to stdout
	fmt.Println(line)

	// Write to file
	if logFile != nil {
		logFile.WriteString(line + "\n")
	}

	// Broadcast to SSE clients
	logClientsMu.RLock()
	for ch := range logClients {
		select {
		case ch <- line:
		default:
			// Client channel full, skip
		}
	}
	logClientsMu.RUnlock()
}

// initDownloadsDB initializes the SQLite database for tracking enabled download chats
func initDownloadsDB() error {
	var err error
	downloadsDB, err = sql.Open("sqlite3", "file:"+cfg.DownloadsDBPath+"?_foreign_keys=on")
	if err != nil {
		return fmt.Errorf("failed to open downloads database: %w", err)
	}

	// Create the download_stats table for tracking download statistics
	_, err = downloadsDB.Exec(`
		CREATE TABLE IF NOT EXISTS download_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_jid TEXT,
			chat_name TEXT,
			url TEXT,
			platform TEXT,
			downloaded_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			success BOOLEAN
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create download_stats table: %w", err)
	}

	// Create table to track which chats have received the welcome message
	_, err = downloadsDB.Exec(`
		CREATE TABLE IF NOT EXISTS welcome_messages_sent (
			chat_jid TEXT PRIMARY KEY,
			chat_name TEXT,
			sent_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create welcome_messages_sent table: %w", err)
	}

	return nil
}

// recordDownload records a download attempt in the statistics table
func recordDownload(chatJID, chatName, url, platform string, success bool) {
	if downloadsDB == nil {
		return
	}
	_, err := downloadsDB.Exec(
		"INSERT INTO download_stats (chat_jid, chat_name, url, platform, success) VALUES (?, ?, ?, ?, ?)",
		chatJID, chatName, url, platform, success,
	)
	if err != nil {
		logBoth("ERROR", "Failed to record download stat: %v", err)
	}
}

// sendTextMessage sends a text message to a chat
func sendTextMessage(chatJID types.JID, text string) error {
	msg := &waProto.Message{
		Conversation: proto.String(text),
	}
	_, err := WamClient.SendMessage(context.Background(), chatJID, msg)
	return err
}

// hasDownloadKeyword checks if text ends with "download" (case insensitive)
func hasDownloadKeyword(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	return strings.HasSuffix(text, "download")
}

// extractURLWithDownload extracts the URL from text that ends with "download"
func extractURLWithDownload(text string) string {
	// Remove the "download" suffix and trim
	text = strings.TrimSpace(text)
	if !hasDownloadKeyword(text) {
		return ""
	}
	// Remove "download" from the end
	text = strings.TrimSpace(text[:len(text)-8])
	return text
}

// isStandaloneDownloadRequest checks if message is just "download" or "download please"
func isStandaloneDownloadRequest(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	return text == "download" || text == "download please" || text == "please download"
}

// detectPlatform returns the platform name for a given URL
func detectPlatform(url string) string {
	if instagramURLRegex.MatchString(url) {
		return "instagram"
	}
	if twitterURLRegex.MatchString(url) {
		return "twitter"
	}
	if facebookURLRegex.MatchString(url) {
		return "facebook"
	}
	if youtubeURLRegex.MatchString(url) {
		return "youtube"
	}
	return "unknown"
}

// extractURLFromText extracts a supported URL from text (Instagram, Twitter, Facebook, or YouTube)
func extractURLFromText(text string) string {
	// Try Instagram
	if urls := extractInstagramURLs(text); len(urls) > 0 {
		return urls[0]
	}
	// Try Twitter
	if urls := extractTwitterURLs(text); len(urls) > 0 {
		return urls[0]
	}
	// Try Facebook
	if urls := extractFacebookURLs(text); len(urls) > 0 {
		return urls[0]
	}
	// Try YouTube
	if urls := extractYouTubeURLs(text); len(urls) > 0 {
		return urls[0]
	}
	return ""
}

// OllamaRequest represents a request to the Ollama API
type OllamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// OllamaResponse represents a response from the Ollama API
type OllamaResponse struct {
	Response string `json:"response"`
}

// Intent represents the detected intent from a user message
type Intent struct {
	Category string `json:"category"` // "download", "control", "chat"
	Command  string `json:"command"`  // For control: "enable", "disable", "status", "help"
	URL      string `json:"url"`      // Extracted URL if any
}

// queryOllama sends a prompt to Ollama and returns the response
func queryOllama(prompt string) (string, error) {
	if !cfg.OllamaEnabled || cfg.OllamaURL == "" {
		return "", fmt.Errorf("Ollama AI is not configured")
	}

	reqBody := OllamaRequest{
		Model:  cfg.OllamaModel,
		Prompt: prompt,
		Stream: false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", cfg.OllamaURL+"/api/generate", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if cfg.OllamaUsername != "" && cfg.OllamaPassword != "" {
		req.SetBasicAuth(cfg.OllamaUsername, cfg.OllamaPassword)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to call Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	var ollamaResp OllamaResponse
	if err := json.Unmarshal(body, &ollamaResp); err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return ollamaResp.Response, nil
}

// detectIntent uses Ollama to classify the user's message intent
func detectIntent(message string) (*Intent, error) {
	// First, check if message contains a URL - if so, it's likely a download intent
	url := extractURLFromText(message)

	systemPrompt := `You are an intent classifier for a WhatsApp bot. Classify the user's message into one of these categories:
- download: User wants to download a video/image from a URL or asks to download something
- control: User wants to control the bot (commands like: enable, disable, status, help, start, stop)
- chat: User wants to have a conversation or ask a question

Respond with ONLY a JSON object, no other text: {"category": "download|control|chat", "command": "enable|disable|status|help|none"}

Examples:
- "download this video" -> {"category": "download", "command": "none"}
- "enable auto downloads" -> {"category": "control", "command": "enable"}
- "help" -> {"category": "control", "command": "help"}
- "what's the weather?" -> {"category": "chat", "command": "none"}
- "https://youtube.com/shorts/xxx" -> {"category": "download", "command": "none"}

User message: ` + message

	response, err := queryOllama(systemPrompt)
	if err != nil {
		return nil, err
	}

	// Try to extract JSON from response
	response = strings.TrimSpace(response)

	// Find JSON in response (it might have extra text)
	startIdx := strings.Index(response, "{")
	endIdx := strings.LastIndex(response, "}")
	if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
		response = response[startIdx : endIdx+1]
	}

	var intent Intent
	if err := json.Unmarshal([]byte(response), &intent); err != nil {
		// If JSON parsing fails, make a best guess based on keywords
		lowerMsg := strings.ToLower(message)
		if strings.Contains(lowerMsg, "help") {
			return &Intent{Category: "control", Command: "help"}, nil
		}
		if strings.Contains(lowerMsg, "enable") {
			return &Intent{Category: "control", Command: "enable"}, nil
		}
		if strings.Contains(lowerMsg, "disable") {
			return &Intent{Category: "control", Command: "disable"}, nil
		}
		if strings.Contains(lowerMsg, "status") {
			return &Intent{Category: "control", Command: "status"}, nil
		}
		if url != "" || strings.Contains(lowerMsg, "download") {
			return &Intent{Category: "download", Command: "none", URL: url}, nil
		}
		// Default to chat
		return &Intent{Category: "chat", Command: "none"}, nil
	}

	intent.URL = url
	return &intent, nil
}

// generateChatResponse uses Ollama to generate a conversational response
func generateChatResponse(message string) (string, error) {
	systemPrompt := `You are SkipTheFeed, a WhatsApp bot that downloads videos and images so users don't have to visit social media sites. Be concise and friendly.

YOUR CAPABILITIES:
- Download videos/images from Instagram (posts, reels, stories)
- Download YouTube videos and Shorts
- Download Twitter/X videos
- Download Facebook videos and reels

HOW TO USE YOU:
- Users just send a link and you automatically download and send it back
- Commands: !help

Keep responses short (1-3 sentences). Don't use markdown. When asked about downloading, tell users to just send the link directly to this chat.

User: ` + message

	response, err := queryOllama(systemPrompt)
	if err != nil {
		return "Sorry, I'm having trouble thinking right now. Try again later!", nil
	}

	// Clean up the response
	response = strings.TrimSpace(response)
	if len(response) > 500 {
		response = response[:500] + "..."
	}

	return response, nil
}

// handleControlCommand handles bot control commands
func handleControlCommand(chatJID types.JID, command string) {
	switch command {
	case "help":
		helpMsg := `*SkipTheFeed*
Watch just the video, skip the feed.

Send me any video link and I'll download it for you!

*Supported Platforms*
- Instagram (posts, reels, stories)
- YouTube (videos, shorts)
- Twitter/X
- Facebook (videos, reels)`
		sendTextMessage(chatJID, helpMsg)
	default:
		sendTextMessage(chatJID, "Just send me a video link and I'll download it!")
	}
}

// processAICommand processes a message that starts with ! trigger
func processAICommand(chatJID types.JID, chatName, message string) {
	logBoth("INFO", "Processing AI command from %s: %s", chatName, message)

	intent, err := detectIntent(message)
	if err != nil {
		logBoth("ERROR", "Failed to detect intent: %v", err)
		sendTextMessage(chatJID, "Sorry, I couldn't understand that. Try !help for commands.")
		return
	}

	logBoth("INFO", "Detected intent: category=%s, command=%s, url=%s", intent.Category, intent.Command, intent.URL)

	switch intent.Category {
	case "download":
		url := intent.URL
		if url == "" {
			url = extractURLFromText(message)
		}
		if url != "" {
			sendTextMessage(chatJID, "Downloading...")
			go downloadAndSendVideo(chatJID, chatName, url)
		} else {
			sendTextMessage(chatJID, "Please provide a URL to download. I support Instagram, YouTube, Twitter, and Facebook.")
		}
	case "control":
		handleControlCommand(chatJID, intent.Command)
	case "chat":
		response, _ := generateChatResponse(message)
		sendTextMessage(chatJID, response)
	default:
		sendTextMessage(chatJID, "I'm not sure what you want. Type !help for available commands.")
	}
}

// downloadToTemp downloads a video to temp directory and returns the file path
func downloadToTemp(url string, platform string) ([]string, error) {
	// Create temp directory if it doesn't exist
	if err := os.MkdirAll(cfg.TempDownloadDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Build yt-dlp arguments
	args := []string{
		"-P", cfg.TempDownloadDir,
		"-o", "%(id)s_%(autonumber)s.%(ext)s",
		"--print", "after_move:filepath",
		// Force H.264 (avc1) codec and mp4 format for WhatsApp compatibility
		// WhatsApp doesn't support AV1 or VP9 codecs
		"-f", "bestvideo[vcodec^=avc1]+bestaudio[acodec^=mp4a]/bestvideo[vcodec^=avc1]+bestaudio/best[vcodec^=avc1]/best",
		"--merge-output-format", "mp4",
	}

	// Add cookies for Instagram to handle authentication
	if platform == "instagram" {
		if _, err := os.Stat(cfg.InstagramCookiesFile); err == nil {
			args = append(args, "--cookies", cfg.InstagramCookiesFile)
		}
	}

	args = append(args, url)

	// Use yt-dlp to download the media
	ytdlpCmd := exec.Command(cfg.YtDlpPath, args...)
	output, err := ytdlpCmd.CombinedOutput()

	// If yt-dlp fails with "No video formats found" for Instagram, try gallery-dl for images
	if err != nil && platform == "instagram" && strings.Contains(string(output), "No video formats found") {
		logBoth("INFO", "yt-dlp found no video formats, trying gallery-dl for images...")
		return downloadWithGalleryDL(url)
	}

	if err != nil {
		return nil, fmt.Errorf("yt-dlp failed: %v, output: %s", err, string(output))
	}

	// Parse all filepaths from output (one per line for carousel posts)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var filepaths []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Only include lines that look like file paths (contain temp download dir)
		if strings.Contains(line, cfg.TempDownloadDir) {
			// Verify file exists
			if _, err := os.Stat(line); err == nil {
				filepaths = append(filepaths, line)
			}
		}
	}

	if len(filepaths) == 0 {
		return nil, fmt.Errorf("yt-dlp did not return any valid filepaths, output: %s", string(output))
	}

	return filepaths, nil
}

// downloadWithGalleryDL uses gallery-dl to download images (fallback for Instagram image posts)
func downloadWithGalleryDL(url string) ([]string, error) {
	args := []string{
		"-D", cfg.TempDownloadDir,
		url,
	}

	// Add cookies if available
	if _, err := os.Stat(cfg.InstagramCookiesFile); err == nil {
		args = append([]string{"--cookies", cfg.InstagramCookiesFile}, args...)
	}

	cmd := exec.Command(cfg.GalleryDlPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gallery-dl failed: %v, output: %s", err, string(output))
	}

	// gallery-dl prints downloaded file paths to stdout
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var filepaths []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Only include lines that look like file paths
		if strings.HasPrefix(line, cfg.TempDownloadDir) || strings.HasPrefix(line, "./"+cfg.TempDownloadDir) {
			// Normalize path
			cleanPath := strings.TrimPrefix(line, "./")
			// Verify file exists
			if _, err := os.Stat(cleanPath); err == nil {
				filepaths = append(filepaths, cleanPath)
			}
		}
	}

	if len(filepaths) == 0 {
		return nil, fmt.Errorf("gallery-dl did not return any valid filepaths, output: %s", string(output))
	}

	return filepaths, nil
}

// sendVideoMessage sends a video file to a chat
func sendVideoMessage(chatJID types.JID, videoPath string) error {
	// Read the video file
	videoData, err := os.ReadFile(videoPath)
	if err != nil {
		return fmt.Errorf("failed to read video file: %w", err)
	}

	// Upload the video to WhatsApp
	uploaded, err := WamClient.Upload(context.Background(), videoData, whatsmeow.MediaVideo)
	if err != nil {
		return fmt.Errorf("failed to upload video: %w", err)
	}

	// Determine mime type based on extension
	ext := strings.ToLower(filepath.Ext(videoPath))
	mimeType := "video/mp4"
	if ext == ".webm" {
		mimeType = "video/webm"
	} else if ext == ".mkv" {
		mimeType = "video/x-matroska"
	} else if ext == ".mov" {
		mimeType = "video/quicktime"
	}

	// Create video message
	msg := &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(videoData))),
		},
	}

	_, err = WamClient.SendMessage(context.Background(), chatJID, msg)
	if err != nil {
		return fmt.Errorf("failed to send video message: %w", err)
	}

	return nil
}

// sendImageMessage sends an image file to a chat
func sendImageMessage(chatJID types.JID, imagePath string) error {
	// Read the image file
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		return fmt.Errorf("failed to read image file: %w", err)
	}

	// Upload the image to WhatsApp
	uploaded, err := WamClient.Upload(context.Background(), imageData, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("failed to upload image: %w", err)
	}

	// Determine mime type based on extension
	ext := strings.ToLower(filepath.Ext(imagePath))
	mimeType := "image/jpeg"
	if ext == ".png" {
		mimeType = "image/png"
	} else if ext == ".gif" {
		mimeType = "image/gif"
	} else if ext == ".webp" {
		mimeType = "image/webp"
	}

	// Create image message
	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(imageData))),
		},
	}

	_, err = WamClient.SendMessage(context.Background(), chatJID, msg)
	if err != nil {
		return fmt.Errorf("failed to send image message: %w", err)
	}

	return nil
}

// isImageFile checks if a file is an image based on extension
func isImageFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp"
}

// isVideoFile checks if a file is a video based on extension
func isVideoFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".mp4" || ext == ".webm" || ext == ".mkv" || ext == ".mov" || ext == ".avi"
}

// shouldSendWelcomeMessage checks if the welcome message should be sent for a specific chat
func shouldSendWelcomeMessage(chatJID string) bool {
	botSettingsMu.RLock()
	enabled := botSettings != nil && botSettings.WelcomeMessageEnabled
	botSettingsMu.RUnlock()

	if !enabled || downloadsDB == nil {
		return false
	}

	// Check if this chat has already received the welcome message
	var count int
	err := downloadsDB.QueryRow("SELECT COUNT(*) FROM welcome_messages_sent WHERE chat_jid = ?", chatJID).Scan(&count)
	if err != nil {
		logBoth("ERROR", "Failed to check welcome message status: %v", err)
		return false
	}

	return count == 0
}

// markWelcomeMessageSent marks the welcome message as sent for a specific chat
func markWelcomeMessageSent(chatJID, chatName string) {
	if downloadsDB == nil {
		return
	}

	_, err := downloadsDB.Exec(
		"INSERT OR REPLACE INTO welcome_messages_sent (chat_jid, chat_name) VALUES (?, ?)",
		chatJID, chatName,
	)
	if err != nil {
		logBoth("ERROR", "Failed to mark welcome message as sent: %v", err)
	}
}

// resetAllWelcomeMessages clears the welcome message sent status for all chats
func resetAllWelcomeMessages() error {
	if downloadsDB == nil {
		return fmt.Errorf("database not initialized")
	}

	_, err := downloadsDB.Exec("DELETE FROM welcome_messages_sent")
	return err
}

// getWelcomeMessagesSentCount returns the number of chats that have received the welcome message
func getWelcomeMessagesSentCount() int {
	if downloadsDB == nil {
		return 0
	}

	var count int
	err := downloadsDB.QueryRow("SELECT COUNT(*) FROM welcome_messages_sent").Scan(&count)
	if err != nil {
		return 0
	}
	return count
}

// getWelcomeMessageText returns the configured welcome message
func getWelcomeMessageText() string {
	botSettingsMu.RLock()
	defer botSettingsMu.RUnlock()
	if botSettings != nil && botSettings.WelcomeMessageText != "" {
		return botSettings.WelcomeMessageText
	}
	return defaultWelcomeMessage
}

// isAutoDownloadEnabled checks if auto-download is enabled
func isAutoDownloadEnabled() bool {
	botSettingsMu.RLock()
	defer botSettingsMu.RUnlock()
	return botSettings != nil && botSettings.AutoDownloadEnabled
}

// downloadAndSendMedia downloads media from URL and sends it to the chat
func downloadAndSendVideo(chatJID types.JID, chatName, url string) {
	platform := detectPlatform(url)
	logBoth("INFO", "Starting download for chat %s (%s): %s [platform: %s]", chatJID.String(), chatName, url, platform)

	// For YouTube, check video size before downloading
	if platform == "youtube" {
		ok, filesize, errMsg := checkYouTubeVideoSize(url)
		if !ok {
			logBoth("WARN", "YouTube video too large (%d bytes): %s", filesize, url)
			sendTextMessage(chatJID, errMsg)
			recordDownload(chatJID.String(), chatName, url, platform, false)
			return
		}
		if filesize > 0 {
			logBoth("INFO", "YouTube video size check passed: %.1fMB", float64(filesize)/(1024*1024))
		}
	}

	// Download the media (may return multiple files for carousel posts)
	mediaPaths, err := downloadToTemp(url, platform)
	if err != nil {
		logBoth("ERROR", "Failed to download media: %v", err)
		sendTextMessage(chatJID, fmt.Sprintf("Failed to download: %v", err))
		recordDownload(chatJID.String(), chatName, url, platform, false)
		return
	}

	// Clean up all temp files after sending
	defer func() {
		for _, path := range mediaPaths {
			os.Remove(path)
		}
	}()

	logBoth("INFO", "Downloaded %d file(s): %v", len(mediaPaths), mediaPaths)

	// Send each media file
	successCount := 0
	for _, mediaPath := range mediaPaths {
		var sendErr error
		if isImageFile(mediaPath) {
			logBoth("INFO", "Sending image: %s", mediaPath)
			sendErr = sendImageMessage(chatJID, mediaPath)
		} else if isVideoFile(mediaPath) {
			logBoth("INFO", "Sending video: %s", mediaPath)
			sendErr = sendVideoMessage(chatJID, mediaPath)
		} else {
			logBoth("WARN", "Unknown media type, trying as video: %s", mediaPath)
			sendErr = sendVideoMessage(chatJID, mediaPath)
		}

		if sendErr != nil {
			logBoth("ERROR", "Failed to send media %s: %v", mediaPath, sendErr)
		} else {
			successCount++
		}
	}

	if successCount == 0 {
		sendTextMessage(chatJID, "Failed to send any media files")
		recordDownload(chatJID.String(), chatName, url, platform, false)
		return
	}

	logBoth("INFO", "Successfully sent %d/%d media files to chat %s (%s)", successCount, len(mediaPaths), chatJID.String(), chatName)
	recordDownload(chatJID.String(), chatName, url, platform, true)

	// Send welcome message if this chat hasn't received one yet
	if shouldSendWelcomeMessage(chatJID.String()) {
		sendTextMessage(chatJID, getWelcomeMessageText())
		markWelcomeMessageSent(chatJID.String(), chatName)
		logBoth("INFO", "Sent welcome message to chat %s (%s)", chatJID.String(), chatName)
	}
}

// checkInstagramCookies checks if Instagram cookies file exists
func checkInstagramCookies() bool {
	if _, err := os.Stat(cfg.InstagramCookiesFile); err == nil {
		logBoth("INFO", "Instagram cookies found at %s", cfg.InstagramCookiesFile)
		return true
	}
	logBoth("WARN", "Instagram cookies not found at %s - Instagram downloads may fail", cfg.InstagramCookiesFile)
	logBoth("INFO", "To enable Instagram downloads, upload a Netscape-format cookies file to %s", cfg.InstagramCookiesFile)
	return false
}

// extractInstagramURLs finds all Instagram URLs in a text message
func extractInstagramURLs(text string) []string {
	matches := instagramURLRegex.FindAllString(text, -1)
	return matches
}

// extractTwitterURLs finds all Twitter/X URLs in a text message
func extractTwitterURLs(text string) []string {
	matches := twitterURLRegex.FindAllString(text, -1)
	return matches
}

// extractFacebookURLs finds all Facebook URLs in a text message
func extractFacebookURLs(text string) []string {
	matches := facebookURLRegex.FindAllString(text, -1)
	return matches
}

// extractYouTubeURLs finds all YouTube URLs in a text message
func extractYouTubeURLs(text string) []string {
	matches := youtubeURLRegex.FindAllString(text, -1)
	return matches
}

// YouTubeVideoInfo contains video metadata from yt-dlp
type YouTubeVideoInfo struct {
	Filesize         int64   `json:"filesize"`
	FilesizeApprox   int64   `json:"filesize_approx"`
	Duration         float64 `json:"duration"`
	Title            string  `json:"title"`
}

// checkYouTubeVideoSize checks if a YouTube video is within size limits
// Returns (isOK, filesize in bytes, error message)
func checkYouTubeVideoSize(url string) (bool, int64, string) {
	// Use yt-dlp to get video info without downloading
	args := []string{
		"-j", // Output JSON
		"--no-download",
		"-f", "bestvideo[vcodec^=avc1]+bestaudio[acodec^=mp4a]/bestvideo[vcodec^=avc1]+bestaudio/best[vcodec^=avc1]/best",
		url,
	}

	cmd := exec.Command(cfg.YtDlpPath, args...)
	output, err := cmd.Output()
	if err != nil {
		// If we can't get info, allow the download attempt anyway
		logBoth("WARN", "Could not get YouTube video info: %v", err)
		return true, 0, ""
	}

	var info YouTubeVideoInfo
	if err := json.Unmarshal(output, &info); err != nil {
		logBoth("WARN", "Could not parse YouTube video info: %v", err)
		return true, 0, ""
	}

	// Use filesize or filesize_approx
	filesize := info.Filesize
	if filesize == 0 {
		filesize = info.FilesizeApprox
	}

	// If we still don't have filesize, check duration as a fallback
	// Assume ~1MB per 10 seconds for a rough estimate
	if filesize == 0 && info.Duration > 0 {
		// Rough estimate: 1MB per 10 seconds (800kbps)
		filesize = int64(info.Duration * 100000) // ~100KB per second estimate
	}

	if filesize > maxYouTubeFileSize {
		sizeMB := float64(filesize) / (1024 * 1024)
		return false, filesize, fmt.Sprintf("Video is too large (%.1fMB). Maximum size is 10MB.", sizeMB)
	}

	return true, filesize, ""
}

func eventHandler(evtInterface interface{}) {

	logLevel := "DEBUG"
	log = waLog.Stdout("Main", logLevel, true)

	switch evt := evtInterface.(type) {
	case *events.Message:
		fmt.Println("Received a message!", evt.Message.GetConversation())
		fmt.Println("Message MediaType:", evt.Info.MediaType, "Type:", evt.Info.Type)
		// fmt.Print("Message: ", v.Message.GetConversation())
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		if evt.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", evt.Info.Type))
		}
		if evt.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", evt.Info.Category))
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "ephemeral")
		}
		if evt.IsViewOnceV2 {
			metaParts = append(metaParts, "ephemeral (v2)")
		}
		if evt.IsDocumentWithCaption {
			metaParts = append(metaParts, "document with caption")
		}
		if evt.IsEdit {
			metaParts = append(metaParts, "edit")
		}

		// Log message receipt to dashboard
		logBoth("INFO", "Message from %s: %s", evt.Info.PushName, evt.Info.SourceString())

		if evt.Message != nil {
			log.Infof("Recieved message: %+v", evt.Message)
		}

		if evt.Message.GetPollUpdateMessage() != nil {
			decrypted, err := WamClient.DecryptPollVote(context.Background(), evt)
			if err != nil {
				log.Errorf("Failed to decrypt vote: %v", err)
			} else {
				log.Infof("Selected options in decrypted vote:")
				for _, option := range decrypted.SelectedOptions {
					log.Infof("- %X", option)
				}
			}
		} else if evt.Message.GetEncReactionMessage() != nil {
			decrypted, err := WamClient.DecryptReaction(context.Background(), evt)
			if err != nil {
				log.Errorf("Failed to decrypt encrypted reaction: %v", err)
			} else {
				log.Infof("Decrypted reaction: %+v", decrypted)
			}
		}

		img := evt.Message.GetImageMessage()
		if img != nil {
			data, err := WamClient.Download(context.Background(), img)
			if err != nil {
				log.Errorf("Failed to download image: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(img.GetMimetype())
			path := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			err = os.WriteFile(path, data, 0600)
			if err != nil {
				log.Errorf("Failed to save image: %v", err)
				return
			}
			log.Infof("Saved image in message to %s", path)
		}

		// Extract message text for command processing
		var messageText string
		if conv := evt.Message.GetConversation(); conv != "" {
			messageText = conv
		} else if extText := evt.Message.GetExtendedTextMessage(); extText != nil {
			messageText = extText.GetText()
		}

		// Handle AI commands triggered with "!" prefix (only from self)
		if evt.Info.IsFromMe && strings.HasPrefix(messageText, "!") {
			command := strings.TrimPrefix(messageText, "!")
			command = strings.TrimSpace(command)
			if command != "" {
				chatJID := evt.Info.Chat
				chatName := evt.Info.PushName
				if chatName == "" {
					chatName = "Self"
				}
				go processAICommand(chatJID, chatName, command)
				return
			}
		}

		// Auto-download any message containing a supported URL (if enabled)
		if messageText != "" && isAutoDownloadEnabled() {
			url := extractURLFromText(messageText)
			if url != "" {
				chatJID := evt.Info.Chat
				chatName := evt.Info.PushName
				if chatName == "" {
					chatName = "Unknown"
				}
				logBoth("INFO", "URL detected from %s, downloading: %s", chatName, url)

				// Delete the original message containing the URL
				go func() {
					_, err := WamClient.RevokeMessage(context.Background(), chatJID, evt.Info.ID)
					if err != nil {
						logBoth("WARN", "Failed to delete original message: %v", err)
					} else {
						logBoth("INFO", "Deleted original message containing URL")
					}
				}()

				// Download and send the media
				go downloadAndSendVideo(chatJID, chatName, url)
				return
			}
		}

	}
}

// Dashboard and API handlers

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(cfg.DashboardDir, "index.html"))
}

// Auth API handlers

// AuthStatusResponse represents auth status
type AuthStatusResponse struct {
	SetupDone    bool   `json:"setupDone"`
	LoggedIn     bool   `json:"loggedIn"`
	Username     string `json:"username,omitempty"`
}

// getSessionFromRequest extracts session token from cookie or header
func getSessionFromRequest(r *http.Request) string {
	// Check cookie first
	cookie, err := r.Cookie("skipthefeed_session")
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}
	// Check Authorization header
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// handleAuthStatus returns current auth status
func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	authConfigMu.RLock()
	setupDone := authConfig != nil && authConfig.SetupDone
	username := ""
	if authConfig != nil {
		username = authConfig.Username
	}
	authConfigMu.RUnlock()

	loggedIn := false
	sessionToken := getSessionFromRequest(r)
	if sessionToken != "" {
		loggedIn = validateSession(sessionToken)
	}

	resp := AuthStatusResponse{
		SetupDone: setupDone,
		LoggedIn:  loggedIn,
	}
	if loggedIn {
		resp.Username = username
	}

	json.NewEncoder(w).Encode(resp)
}


// LoginRequest represents login credentials
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleAuthLogin handles user login
func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authConfigMu.RLock()
	if !authConfig.SetupDone {
		authConfigMu.RUnlock()
		http.Error(w, "Setup not completed", http.StatusBadRequest)
		return
	}
	storedUsername := authConfig.Username
	storedHash := authConfig.PasswordHash
	authConfigMu.RUnlock()

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Validate credentials
	if req.Username != storedUsername || hashPassword(req.Password) != storedHash {
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}

	// Create session
	sessionToken := createSession()

	logBoth("INFO", "User logged in: %s", req.Username)

	// Set session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "skipthefeed_session",
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400, // 24 hours
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"message": "Login successful",
	})
}

// handleAuthLogout handles user logout
func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	sessionToken := getSessionFromRequest(r)
	if sessionToken != "" {
		deleteSession(sessionToken)
	}

	// Clear cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "skipthefeed_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"message": "Logged out",
	})
}

// authMiddleware protects routes that require authentication
func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authConfigMu.RLock()
		setupDone := authConfig != nil && authConfig.SetupDone
		authConfigMu.RUnlock()

		// If setup not done, allow access (need to complete setup first)
		if !setupDone {
			next(w, r)
			return
		}

		// Check for valid session
		sessionToken := getSessionFromRequest(r)
		if sessionToken == "" || !validateSession(sessionToken) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// StatusResponse represents the bot status
type StatusResponse struct {
	Connected           bool   `json:"connected"`
	Authenticated       bool   `json:"authenticated"`
	QRCode              string `json:"qrCode,omitempty"`
	InstagramCookiesSet bool   `json:"instagramCookiesSet"`
}

func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	connected := WamClient != nil && WamClient.IsConnected()

	// Check Instagram cookies
	instagramCookiesExist := false
	if _, err := os.Stat(cfg.InstagramCookiesFile); err == nil {
		instagramCookiesExist = true
	}

	// Get current QR code if available
	qrCodeMu.RLock()
	qr := currentQRCode
	auth := isAuthenticated
	qrCodeMu.RUnlock()

	resp := StatusResponse{
		Connected:           connected,
		Authenticated:       auth,
		QRCode:              qr,
		InstagramCookiesSet: instagramCookiesExist,
	}

	json.NewEncoder(w).Encode(resp)
}

// SettingsResponse includes bot settings plus welcome message stats
type SettingsResponse struct {
	AutoDownloadEnabled      bool   `json:"autoDownloadEnabled"`
	WelcomeMessageEnabled    bool   `json:"welcomeMessageEnabled"`
	WelcomeMessageText       string `json:"welcomeMessageText"`
	WelcomeMessagesSentCount int    `json:"welcomeMessagesSentCount"`
}

// handleAPIGetSettings returns bot settings
func handleAPIGetSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	botSettingsMu.RLock()
	settings := botSettings
	botSettingsMu.RUnlock()

	resp := SettingsResponse{
		AutoDownloadEnabled:      true,
		WelcomeMessageEnabled:    true,
		WelcomeMessageText:       defaultWelcomeMessage,
		WelcomeMessagesSentCount: getWelcomeMessagesSentCount(),
	}

	if settings != nil {
		resp.AutoDownloadEnabled = settings.AutoDownloadEnabled
		resp.WelcomeMessageEnabled = settings.WelcomeMessageEnabled
		resp.WelcomeMessageText = settings.WelcomeMessageText
	}

	json.NewEncoder(w).Encode(resp)
}

// handleAPIUpdateSettings updates bot settings
func handleAPIUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var newSettings BotSettings
	if err := json.NewDecoder(r.Body).Decode(&newSettings); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	botSettingsMu.Lock()
	if botSettings == nil {
		botSettings = &BotSettings{}
	}
	botSettings.AutoDownloadEnabled = newSettings.AutoDownloadEnabled
	botSettings.WelcomeMessageEnabled = newSettings.WelcomeMessageEnabled
	botSettings.WelcomeMessageText = newSettings.WelcomeMessageText
	botSettingsMu.Unlock()

	if err := saveBotSettings(); err != nil {
		http.Error(w, "Failed to save settings", http.StatusInternalServerError)
		return
	}

	logBoth("INFO", "Bot settings updated")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Settings saved"})
}

// handleAPIResetWelcome resets the welcome message sent status for all chats
func handleAPIResetWelcome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := resetAllWelcomeMessages(); err != nil {
		http.Error(w, "Failed to reset welcome messages", http.StatusInternalServerError)
		return
	}

	logBoth("INFO", "Welcome messages reset - will send again to all chats")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Welcome message will be sent again to all chats"})
}

// handleAPIUploadCookies handles Instagram cookies file upload
func handleAPIUploadCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Validate it looks like a Netscape cookies file
	content := string(body)
	if !strings.Contains(content, "instagram.com") {
		http.Error(w, "Invalid cookies file: must contain Instagram cookies", http.StatusBadRequest)
		return
	}

	// Ensure config directory exists
	configDir := filepath.Dir(cfg.InstagramCookiesFile)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		http.Error(w, "Failed to create config directory", http.StatusInternalServerError)
		return
	}

	// Write cookies file
	if err := os.WriteFile(cfg.InstagramCookiesFile, body, 0600); err != nil {
		http.Error(w, "Failed to save cookies file", http.StatusInternalServerError)
		return
	}

	logBoth("INFO", "Instagram cookies uploaded successfully")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Cookies saved successfully"})
}

// handleAPIDeleteCookies deletes Instagram cookies file
func handleAPIDeleteCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := os.Remove(cfg.InstagramCookiesFile); err != nil && !os.IsNotExist(err) {
		http.Error(w, "Failed to delete cookies file", http.StatusInternalServerError)
		return
	}

	logBoth("INFO", "Instagram cookies deleted")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Cookies deleted"})
}

// InstagramSessionRequest represents the session ID submission
type InstagramSessionRequest struct {
	SessionID string `json:"sessionId"`
}

// handleAPIInstagramSession handles Instagram session ID submission
func handleAPIInstagramSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req InstagramSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		http.Error(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	// Generate a Netscape format cookies file from the session ID
	cookiesContent := fmt.Sprintf(`# Netscape HTTP Cookie File
# https://curl.haxx.se/rfc/cookie_spec.html
# This file was generated by SkipTheFeed

.instagram.com	TRUE	/	TRUE	0	sessionid	%s
`, sessionID)

	// Ensure config directory exists
	configDir := filepath.Dir(cfg.InstagramCookiesFile)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		http.Error(w, "Failed to create config directory", http.StatusInternalServerError)
		return
	}

	// Write cookies file
	if err := os.WriteFile(cfg.InstagramCookiesFile, []byte(cookiesContent), 0600); err != nil {
		http.Error(w, "Failed to save session", http.StatusInternalServerError)
		return
	}

	logBoth("INFO", "Instagram session ID saved successfully")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Session saved successfully"})
}

// UserStats represents download stats for a single user
type UserStats struct {
	ChatName  string   `json:"chatName"`
	Count     int      `json:"count"`
	Platforms []string `json:"platforms"`
}

// StatsResponse represents today's download statistics
type StatsResponse struct {
	Total      int            `json:"total"`
	ByUser     []UserStats    `json:"byUser"`
	ByPlatform map[string]int `json:"byPlatform"`
}

func handleAPIStatsToday(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	resp := StatsResponse{
		ByUser:     []UserStats{},
		ByPlatform: make(map[string]int),
	}

	if downloadsDB == nil {
		json.NewEncoder(w).Encode(resp)
		return
	}

	// Get total downloads today
	downloadsDB.QueryRow(`
		SELECT COUNT(*) FROM download_stats
		WHERE date(downloaded_at) = date('now') AND success = 1
	`).Scan(&resp.Total)

	// Get downloads by platform today
	rows, err := downloadsDB.Query(`
		SELECT platform, COUNT(*) FROM download_stats
		WHERE date(downloaded_at) = date('now') AND success = 1
		GROUP BY platform
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var platform string
			var count int
			if rows.Scan(&platform, &count) == nil {
				resp.ByPlatform[platform] = count
			}
		}
	}

	// Get downloads by user today with their platforms
	rows, err = downloadsDB.Query(`
		SELECT chat_name, COUNT(*) as cnt, GROUP_CONCAT(DISTINCT platform) as platforms
		FROM download_stats
		WHERE date(downloaded_at) = date('now') AND success = 1
		GROUP BY chat_jid
		ORDER BY cnt DESC
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var chatName string
			var count int
			var platforms string
			if rows.Scan(&chatName, &count, &platforms) == nil {
				userStat := UserStats{
					ChatName:  chatName,
					Count:     count,
					Platforms: strings.Split(platforms, ","),
				}
				resp.ByUser = append(resp.ByUser, userStat)
			}
		}
	}

	json.NewEncoder(w).Encode(resp)
}


func handleAPILogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Create client channel
	clientChan := make(chan string, 100)

	// Register client
	logClientsMu.Lock()
	logClients[clientChan] = true
	logClientsMu.Unlock()

	// Cleanup on disconnect
	defer func() {
		logClientsMu.Lock()
		delete(logClients, clientChan)
		logClientsMu.Unlock()
		close(clientChan)
	}()

	// Send initial connection message
	fmt.Fprintf(w, "data: {\"timestamp\": \"%s\", \"level\": \"INFO\", \"message\": \"Connected to log stream\"}\n\n",
		time.Now().Format("2006-01-02 15:04:05"))
	flusher.Flush()

	// Stream logs
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-clientChan:
			// Parse the log line and format as JSON
			fmt.Fprintf(w, "data: %s\n\n", formatLogAsJSON(msg))
			flusher.Flush()
		}
	}
}

func formatLogAsJSON(logLine string) string {
	// Parse log line format: [timestamp] [level] message
	// Return JSON format
	parts := strings.SplitN(logLine, "] ", 3)
	if len(parts) >= 3 {
		timestamp := strings.TrimPrefix(parts[0], "[")
		level := strings.TrimPrefix(parts[1], "[")
		message := parts[2]
		jsonStr, _ := json.Marshal(map[string]string{
			"timestamp": timestamp,
			"level":     level,
			"message":   message,
		})
		return string(jsonStr)
	}
	// Fallback: return as-is with current timestamp
	jsonStr, _ := json.Marshal(map[string]string{
		"timestamp": time.Now().Format("2006-01-02 15:04:05"),
		"level":     "INFO",
		"message":   logLine,
	})
	return string(jsonStr)
}

func handleHealthRequest(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "OK\n")
}

type SendMessageRequest struct {
	To      string `json:"to"`
	Message string `json:"message"`
}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
			log.Errorf("Invalid JID %s: %v", arg, err)
			return recipient, false
		} else if recipient.User == "" {
			log.Errorf("Invalid JID %s: no server specified", arg)
			return recipient, false
		}
		return recipient, true
	}
}

func handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var payload SendMessageRequest
	err := json.NewDecoder(r.Body).Decode(&payload)
	if err != nil {
		log.Infof("Error decoding request body %s", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Printf("payload: %s", payload)

	// example payload: {"to": "123456789@s.whatsapp.net", "message": "hello world"}
	recipient, ok := parseJID(payload.To)
	if !ok {
		log.Infof("Invalid recipient %s", payload.To)
		http.Error(w, "Invalid recipient", http.StatusBadRequest)
		return
	}
	disappearingMode := &waProto.DisappearingMode{
		Initiator:     (*waProto.DisappearingMode_Initiator)(proto.Int32(1)),
		Trigger:       (*waProto.DisappearingMode_Trigger)(proto.Int32(2)),
		InitiatedByMe: proto.Bool(true),
	}

	contextInfo := &waProto.ContextInfo{
		Expiration:              proto.Uint32(86400),
		EntryPointConversionApp: proto.String("whatsapp"),
		DisappearingMode:        disappearingMode,
	}
	extendedTextMsg := &waProto.ExtendedTextMessage{
		Text:        proto.String(payload.Message),
		ContextInfo: contextInfo,
	}
	msg := &waProto.Message{ExtendedTextMessage: extendedTextMsg}
	response, err := WamClient.SendMessage(context.Background(), recipient, msg)
	if err != nil {
		log.Infof("Error sending message %s", err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Printf("response: %s", response)
	io.WriteString(w, "message sent\n")
}

func main() {
	fmt.Println("SkipTheFeed - Watch just the video, skip the feed")

	// Load configuration
	cfg = LoadConfig()
	fmt.Printf("Data directory: %s\n", cfg.DataDir)
	fmt.Printf("Server port: %s\n", cfg.Port)

	// Create all necessary directories
	if err := cfg.EnsureDirectories(); err != nil {
		fmt.Printf("Error creating directories: %v\n", err)
		os.Exit(1)
	}

	// Initialize logging
	if err := initLogging(); err != nil {
		fmt.Printf("Warning: Failed to initialize file logging: %v\n", err)
	} else {
		fmt.Printf("File logging initialized (%s)\n", cfg.LogFilePath)
		defer logFile.Close()
	}

	// Initialize downloads database
	if err := initDownloadsDB(); err != nil {
		fmt.Printf("Warning: Failed to initialize downloads database: %v\n", err)
		fmt.Println("Download-on-request feature will not work.")
	} else {
		fmt.Println("Downloads database initialized successfully.")
	}

	// Check for Instagram cookies
	checkInstagramCookies()

	// Load authentication configuration
	if err := loadAuthConfig(); err != nil {
		fmt.Printf("Warning: Failed to load auth config: %v\n", err)
	}

	// Load bot settings
	if err := loadBotSettings(); err != nil {
		fmt.Printf("Warning: Failed to load bot settings: %v\n", err)
	}

	dbLog := waLog.Stdout("Database", "DEBUG", true)
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:"+cfg.WhatsAppDBPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}
	version := store.WAVersionContainer{121, 0, 0}
	store.SetOSInfo("Firefox (Ubuntu)", version)
	// If you want multiple sessions, remember their JIDs and use .GetDevice(jid) or .GetAllDevices() instead.
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		panic(err)
	}

	clientLog := waLog.Stdout("Client", "DEBUG", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	WamClient = client
	client.AddEventHandler(eventHandler)

	// Setup HTTP server (start it regardless of auth status)
	router := mux.NewRouter()

	// Public endpoints (no auth required)
	router.HandleFunc("/", handleDashboard).Methods("GET")
	router.HandleFunc("/health", handleHealthRequest).Methods("GET")

	// Auth endpoints (no auth required)
	router.HandleFunc("/api/auth/status", handleAuthStatus).Methods("GET")
	router.HandleFunc("/api/auth/login", handleAuthLogin).Methods("POST")
	router.HandleFunc("/api/auth/logout", handleAuthLogout).Methods("POST")

	// Protected API endpoints (auth required)
	router.HandleFunc("/api/status", authMiddleware(handleAPIStatus)).Methods("GET")
	router.HandleFunc("/api/stats/today", authMiddleware(handleAPIStatsToday)).Methods("GET")
	router.HandleFunc("/api/logs", authMiddleware(handleAPILogs)).Methods("GET")
	router.HandleFunc("/api/cookies", authMiddleware(handleAPIUploadCookies)).Methods("POST")
	router.HandleFunc("/api/cookies", authMiddleware(handleAPIDeleteCookies)).Methods("DELETE")
	router.HandleFunc("/api/instagram-session", authMiddleware(handleAPIInstagramSession)).Methods("POST")
	router.HandleFunc("/api/settings", authMiddleware(handleAPIGetSettings)).Methods("GET")
	router.HandleFunc("/api/settings", authMiddleware(handleAPIUpdateSettings)).Methods("POST")
	router.HandleFunc("/api/settings/reset-welcome", authMiddleware(handleAPIResetWelcome)).Methods("POST")
	router.HandleFunc("/sendmessage", authMiddleware(handleSendMessage)).Methods("POST")

	// Start HTTP server in background
	go func() {
		fmt.Printf("Starting web server on port %s...\n", cfg.Port)
		fmt.Printf("Dashboard: http://localhost:%s\n", cfg.Port)
		if err := http.ListenAndServe(":"+cfg.Port, router); err != nil {
			logBoth("ERROR", "HTTP server error: %v", err)
		}
	}()

	if client.Store.ID == nil {
		// No ID stored, new login required
		fmt.Printf("WhatsApp not authenticated. Scan the QR code at http://localhost:%s\n", cfg.Port)
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				// Store QR code for web display
				qrCodeMu.Lock()
				currentQRCode = evt.Code
				isAuthenticated = false
				qrCodeMu.Unlock()
				logBoth("INFO", "QR code ready - scan it from the web dashboard")
			} else if evt.Event == "success" {
				logBoth("INFO", "WhatsApp login successful!")
				qrCodeMu.Lock()
				currentQRCode = ""
				isAuthenticated = true
				qrCodeMu.Unlock()
			} else {
				logBoth("DEBUG", "Login event: %s", evt.Event)
			}
		}
	} else {
		// Already logged in, just connect
		qrCodeMu.Lock()
		isAuthenticated = true
		qrCodeMu.Unlock()

		err = client.Connect()
		if err != nil {
			panic(err)
		}
		fmt.Println("WhatsApp connected successfully!")
	}

	// Listen to Ctrl+C (you can also do something else that prevents the program from exiting)
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()

}
