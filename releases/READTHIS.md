# Go Audio Streamer - Pre-compiled Binaries

These are the pre-compiled binaries for Go Audio Streamer. You don't need to install Go to run these, but you **MUST** have FFmpeg installed.

## 🚀 How to use

1. **Download** the binary for your operating system:
   - Linux (amd64/arm64)
   - Windows (amd64)
   - macOS (Intel/Silicon)

2. **Install FFmpeg**:
   Make sure `ffmpeg` is installed and accessible from your terminal/command prompt.
   Download it from: https://www.ffmpeg.org/download.html

3. **Prepare Configuration**:
   Create a `station.cfg` file (see the main README for example).

4. **Run the Streamer**:
   - **Linux/macOS**:
     ```bash
     chmod +x streamer-linux-amd64
     ./streamer-linux-amd64 -port 8080 -config station.cfg
     ```
   - **Windows**:
     ```cmd
     streamer-windows-amd64.exe -port 8080 -config station.cfg
     ```

## 🎧 Features Included
- 8-Channel Dynamic Mixer
- HLS Adaptive Bitrate (AAC/Opus)
- Icecast to HLS Relaying support
- Port 80 Station Shortcuts

For more details, visit the main repository: [github.com/hrizal/GO-STREAMER](https://github.com/hrizal/GO-STREAMER)
