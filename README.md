# SkipTheFeed

**Watch just the video, skip the feed.**

SkipTheFeed is a WhatsApp bot that downloads videos from social media links so you don't have to visit those sites. When someone shares an Instagram reel or YouTube short, you get just the video - no algorithmic feeds, no endless scrolling, no "just one more" rabbit holes.

> **⚠️ Security Warning**
> - **Run locally only** - Never expose this application to the internet
> - **Full message access** - This bot has access to ALL your WhatsApp messages
> - **Trust required** - Only run on devices and networks you trust

## Why?

Social media platforms are designed to keep you scrolling. You click a link to watch a 30-second video and emerge 45 minutes later wondering where the time went. SkipTheFeed breaks that cycle:

1. Someone shares a link in WhatsApp
2. The bot downloads just that video
3. You watch it right in WhatsApp
4. You never visit the site, never see the feed, never get sucked in

**Your attention stays yours.**

## Features

- **Multi-Platform Support**: Instagram, Twitter/X, Facebook, and YouTube
- **Auto-Download**: Automatically downloads videos when links are shared
- **Auto-Delete**: Removes the original link message to keep chats clean
- **Web Dashboard**: Monitor activity and configure settings
- **Portal Authentication**: Secure dashboard with auto-generated credentials
- **Instagram Session**: Easy Instagram login via session ID

## Quick Start with Docker

1. Clone the repository:
```bash
git clone https://github.com/sandeep-sidhu/skipthefeed.git
cd skipthefeed
```

2. Start the container:
```bash
docker compose up -d
```

3. Check the logs for your portal credentials:
```bash
docker compose logs | grep -A5 "PORTAL CREDENTIALS"
```

4. Open the dashboard at `http://localhost:3333` and login

5. Scan the WhatsApp QR code with your phone

6. (Optional) Add Instagram session ID for private content

## How It Works

1. Someone sends a video link (Instagram, YouTube, Twitter/X, or Facebook) to any chat
2. SkipTheFeed automatically downloads the video
3. The original message with the link is deleted
4. The video is sent directly in the chat

No app switching. No feeds. No doomscrolling.

## Supported Platforms

| Platform | Content Types |
|----------|---------------|
| **Instagram** | Posts, Reels, Stories |
| **YouTube** | Videos, Shorts (max 10MB) |
| **Twitter/X** | Video tweets |
| **Facebook** | Public videos |

## Configuration

SkipTheFeed is configured via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `SKIPTHEFEED_DATA_DIR` | `./data` | Directory for data storage |
| `SKIPTHEFEED_PORT` | `3333` | Dashboard port |
| `YTDLP_PATH` | `yt-dlp` | Path to yt-dlp |
| `GALLERYDL_PATH` | `gallery-dl` | Path to gallery-dl |

## Instagram Authentication

For private Instagram content:

1. Open [instagram.com](https://instagram.com) and log in
2. Press F12 to open Developer Tools
3. Navigate to:
   - **Firefox**: Storage → Cookies → instagram.com
   - **Chrome**: Application → Cookies → instagram.com
4. Find `sessionid` and copy its value
5. Paste it in the dashboard under Instagram Setup

## Dashboard

Access at `http://localhost:3333`:

- Real-time activity logs
- Download statistics
- WhatsApp QR code authentication
- Instagram session configuration

### Portal Credentials

On first run, SkipTheFeed creates an admin account:

```
╔════════════════════════════════════════════════════════════╗
║                    PORTAL CREDENTIALS                      ║
╠════════════════════════════════════════════════════════════╣
║  Username: admin                                          ║
║  Password: silver-purple-victor                           ║
╚════════════════════════════════════════════════════════════╝
```

Credentials are displayed on every container start.

## Development

For hot-reloading during development:

```bash
docker compose -f docker-compose-dev.yml up --build
```

Uses [air](https://github.com/air-verse/air) for automatic rebuilds on file changes.

## Manual Installation

### Prerequisites

- Go 1.23+
- [yt-dlp](https://github.com/yt-dlp/yt-dlp)
- [gallery-dl](https://github.com/mikf/gallery-dl)
- FFmpeg
- SQLite

### Build & Run

```bash
go build -o skipthefeed .
./skipthefeed
```

## Troubleshooting

| Issue | Solution |
|-------|----------|
| QR code not showing | Refresh dashboard, check logs |
| Instagram downloads failing | Update session ID, check expiry |
| Video not sending | Check file size limits, verify FFmpeg installed |
| Message not deleted | Bot needs admin rights in groups |

## License

MIT License

## Acknowledgments

- [whatsmeow](https://github.com/tulir/whatsmeow) - WhatsApp Web API
- [yt-dlp](https://github.com/yt-dlp/yt-dlp) - Video downloader
- [gallery-dl](https://github.com/mikf/gallery-dl) - Media downloader
