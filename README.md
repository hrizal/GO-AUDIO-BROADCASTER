# Go Audio Streamer

A high-performance audio streaming server written in Go. It supports real-time audio mixing, HLS adaptive bitrate streaming, and dynamic station management via REST API.

## Features

- **HLS Adaptive Bitrate**: Serves audio in multiple bitrates (64k, 96k, 128k) using HLS.
- **Dynamic Mixing**: Supports 8-channel audio mixing with automatic ducking and crossfading.
- **REST API**: Full control over stations, queues, and playback settings via HTTP endpoints.
- **Low Latency**: Optimized FFmpeg pipeline for smooth streaming.
- **Stability**: Automated HLS segment rotation and cleanup.
- **Loudness Normalization**: Built-in EBU R128 (`loudnorm`) normalization for consistent audio volume across all tracks.
- **Health Monitoring**: Built-in health check and real-time station status.
- **Easy Station Shortcuts**: Automatic listener on port 80 (if available) for easy access: `http://yourip/station_id/`. Can be disabled using `-shortcuts=false`.
- **Wide Format Support**: Supports MP3, WAV, OGG, FLAC, AAC, M4A, and WMA.
- **Icecast/Shoutcast Relay**: Can pull remote streams and convert them to HLS in real-time.

## 🎧 Advanced Audio Engine

### 1. Auto-DJ & Smooth Crossfading
The streamer features an intelligent playback loop that acts as a 24/7 Auto-DJ. It automatically manages transitions between tracks:
- **3-Second Crossfade**: When transitioning between regular playlist tracks, the engine overlaps the audio for 3 seconds to ensure a professional, gapless radio experience.
- **Gapless Playback**: No silence between songs.

### 2. 8-Channel Dynamic Mixer
Built-in software mixer with 8 independent input channels:
- **Channel 0 (Priority/Insert)**: Used for bumpers, jingles, and ads.
- **Channels 1 & 2**: Used for the main music rotation (alternating for crossfades).
- **Channels 3-7**: Available for instant play, background loops, or secondary audio sources.
- **Real-time Control**: Every channel's volume and mute status can be adjusted individually via the REST API while the stream is live.

### 3. Automatic Ducking
When a track is played on the **Priority Channel (Channel 0)**, the mixer can automatically "duck" (lower the volume) of all other active channels. This is perfect for radio announcers or voice-overs over background music.

### 4. Icecast/Shoutcast to HLS Relay
You can use Go Audio Streamer as a powerful relay. Instead of local files, you can inject a network URL:
- **How it works**: Simply send the URL of an existing Icecast/Shoutcast stream to the `/inject` API. 
- **Use Case**: Pull a live stream from a remote studio and serve it to thousands of listeners via HLS + CDN.

```bash
curl -X POST http://localhost:8080/inject \
  -d '{
    "station_id": "radio1",
    "type": "playlist",
    "files": ["http://icecast-server.com:8000/live.mp3"]
  }'
```

### 💡 Pro Tip: Multi-Station "Simulcast" Broadcaster
Since Go Audio Streamer supports multiple independent stations, you can create a radio network with different genres (e.g., `pop`, `jazz`, `rock`) and have **one announcer speak on all of them simultaneously**.
- **How**: Send a `/breaking` API request to all station IDs with the same announcer audio file. 
- **Effect**: All stations will automatically "duck" their respective music genres and broadcast the announcer's voice at the exact same time.

## Quick Start

### 1. Requirements
- Go 1.22+
- FFmpeg (with `libopus` and `aac` support)

### 2. Installation
```bash
git clone https://github.com/yourusername/streamer.git
cd streamer
go build -o streamer main.go
```

## 🚀 Quick Installation (Recommended)

### Linux (Ubuntu/Debian/CentOS)
If you want to install everything (**FFmpeg**, Binary, and Systemd Service) in one go:
```bash
git clone https://github.com/yourusername/streamer.git
cd streamer
sudo bash quick-install.sh
```

### Windows
1. Run `setup-windows.bat`. It will automatically download and setup **FFmpeg** if it's missing.
2. Start the streamer using the binary in the `releases/` folder.

## 📦 Pre-compiled Binaries
You can download pre-compiled binaries for your platform from the [releases](./releases) folder:
- **Linux**: `streamer-linux-amd64`, `streamer-linux-arm64`
- **Windows**: `streamer-windows-amd64.exe`
- **macOS**: `streamer-darwin-amd64` (Intel), `streamer-darwin-arm64` (Apple Silicon)

*Note: Ensure FFmpeg is installed and available in your system's PATH.*

### 4. Configuration
Edit `station.cfg` to define your radio stations:
```ini
# station_id  output_path  playlist_path [bitrate_flags]
radio1  output=/var/www/hls/radio1  playlist=/home/user/music  aac128=true hls_time=2
radio2  output=/var/www/hls/radio2  playlist=/home/user/jazz   aac128=true hls_time=10
```

### 4. Run
```bash
./streamer -port 8080 -config station.cfg
```

## API Documentation
Detailed API documentation can be found in [API.md](./API.md).

### Basic Usage Examples

**Check Status:**
```bash
curl http://localhost:8080/status?station_id=radio1
```

**Inject Audio:**
```bash
curl -X POST http://localhost:8080/inject \
  -H "Content-Type: application/json" \
  -d '{
    "station_id": "radio1",
    "type": "playlist",
    "files": ["/absolute/path/to/song.mp3"]
  }'
```

## Resource Usage & Scaling

The Go Audio Streamer is designed to be lightweight and efficient. You can further reduce CPU usage by disabling bitrates you don't need in your configuration.

### Typical Resource Consumption (Per Station)
For a single station running with all 6 variants (3 AAC, 3 Opus):
- **CPU**: ~10% to 20% of a single modern CPU core.
- **RAM**: ~50MB to 120MB.
- **Optimization**: Disabling unused bitrates (e.g., only keeping 128k AAC) significantly reduces CPU load.
- **Disk I/O**: Low. HLS segments are small and continuously rotated.

### Minimum Recommended Specs (VPS)
On a entry-level VPS (1 Core, 1GB RAM, 1Gbps Port):
- **Capacity**: Can comfortably run 3 to 5 active stations simultaneously.
- **Listener Capacity**: Since the audio is delivered via HLS (static files), the number of listeners is **NOT** limited by the CPU, but by your **Network Bandwidth**.
    - **100 Mbps Upload**: ~700 concurrent listeners at 128kbps.
    - **1 Gbps Upload**: ~7,000+ concurrent listeners at 128kbps.

### Scaling
- **More Stations**: Increase your CPU cores. FFmpeg is the primary CPU consumer.
- **More Listeners**: Increase your network bandwidth or use a CDN (Cloudflare, CloudFront, etc.) to cache the HLS segments.
    - **Note on CDN**: While live streams are ephemeral, a CDN is highly effective for "request collapsing." If 1,000 listeners request the same 10-second audio segment simultaneously, the CDN fetches it once from your origin and serves it 1,000 times from its edge, saving massive bandwidth on your server.

### ⚡ RAM Disk (tmpfs) Optimization
For "Low Latency" setups (e.g., 2-second segments), it is highly recommended to use a **RAM Disk** to store the HLS segments. This prevents excessive SSD wear and provides near-zero I/O latency.

**How to set up on Linux:**
1. Create a mount point: `sudo mkdir -p /var/www/hls`
2. Mount it as tmpfs (e.g., 512MB):
   ```bash
   sudo mount -t tmpfs -o size=512M tmpfs /var/www/hls
   ```
3. To make it persistent, add this to `/etc/fstab`:
   ```bash
   tmpfs /var/www/hls tmpfs defaults,size=512M 0 0
   ```

### 🚀 Ultimate Scaling: The 500,000+ Listeners Challenge
Believe it or not, you can serve **500,000 concurrent listeners** using a tiny **1 Core / 1GB RAM VPS**. How?
1. **The Engine**: Your 1 Core VPS handles the audio encoding (FFmpeg) and generates HLS segments. This uses ~15% CPU.
2. **The Shield**: You put the streamer behind **Cloudflare** (or any CDN).
3. **The Magic**: When 500,000 people listen, they aren't hitting your VPS. They hit Cloudflare's edge servers. Cloudflare fetches the latest 10-second audio segment from your VPS **only once** and distributes it to all 500,000 listeners.
4. **The Result**: Your VPS bandwidth usage remains at ~128kbps (the cost of 1 stream), while Cloudflare serves **64 Gbps** of traffic to the world.

**Go Audio Streamer + CDN = Infinite Scalability.**

## Systemd Service
A template for systemd service is provided in `streamer.service`. Use the `install-service.sh` script to set it up on Linux servers.

## Security Warning

**CRITICAL:** By default, this application has **no authentication** on its API endpoints. 

- The default bind address is typically `localhost` or specific internal IPs. 
- If you change the binding to `0.0.0.0` or expose the port (8080) to the public internet, **anyone** will be able to control your radio stations, inject files, and change configurations.
- It is strongly recommended to:
    - Use a Firewall (like `ufw`) to restrict access.
    - **Use a Middleware/Backend**: Instead of calling this API directly from a browser, create a simple backend (using PHP, Node.js, Python, etc.) on the same server. Your backend can handle authentication and logic, then communicate with the Streamer API via `localhost`.
    - **Reverse Proxy (Nginx/Apache)**: For production and HTTPS (Port 443), it is highly recommended to use a reverse proxy. See:
        - [nginx.conf.example](./nginx.conf.example)
        - [apache.conf.example](./apache.conf.example)
    - **Tunneling Services (for Home Users)**: If you don't have a public IP or cannot open ports, you can use a tunnel to expose your streamer:
        - **Cloudflare Tunnel**: Recommended for stability and free HTTPS.
        - **Ngrok**: `ngrok http 8080` (quick and easy).
        - **Localtunnel**: `lt --port 8080` (free alternative).
    - Run the streamer behind a Reverse Proxy (e.g., Nginx) with Basic Auth or Token validation.
    - Keep the API port closed to the public internet.

## Support the Project

If you find this project useful and want to support its development, you can buy me a coffee!

[![support](https://ko-fi.com/img/githubbutton_sm.svg)](https://ko-fi.com/gostream)

## License
Apache License 2.0. See [LICENSE](./LICENSE) for details.
