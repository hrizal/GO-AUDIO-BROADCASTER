# Go Audio Broadcaster — API Documentation

Base URL: `http://{server}:8080`

## 1. Check Station Status

Returns the real-time status of a station. **`station_id` is required**.

```
GET /status?station_id={station_id}
```

**Response `200 OK`:**

```json
{
    "timestamp": "2026-05-14T05:30:00Z",
    "status": {
        "station_id": "musikita",
        "status": "playing",
        "previous_track": "Song A.mp3",
        "current_track": "Song B.mp3",
        "next_track": "Song C.mp3",
        "config": {
            "random": false,
            "loop": true,
            "unique": true,
            "aac64": true,
            "aac96": true,
            "aac128": true,
            "opus32": true,
            "opus64": true,
            "opus96": true
        },
        "insert_queue_length": 0,
        "playlist_queue_length": 3
    }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `timestamp` | string (RFC3339) | Response time |
| `status.station_id` | string | Station ID |
| `status.status` | string | `"playing"` or `"silent"` |
| `status.previous_track` | string | Previous track (empty if first) |
| `status.current_track` | string | Currently playing track |
| `status.next_track` | string | Next track in queue (from insert/playlist) |
| `status.config.random` | bool | Shuffle mode (`true`) or sequential (`false`) |
| `status.config.loop` | bool | Loop playlist when finished |
| `status.config.unique` | bool | Do not play the same file twice (in random mode) |
| `status.insert_queue_length` | int | Priority queue length (insert) |
| `status.playlist_queue_length` | int | Normal queue length (playlist) |

**Error if station_id is missing:**
```
HTTP 400: station_id query parameter required
```

**cURL:**
```bash
curl "http://localhost:8080/status?station_id=musikita"
```

---

## 2. Inject Audio to Queue

Adds audio files to a station's queue. Files must already exist on the server.

```
POST /inject
Content-Type: application/json
```

**Request Body:**

```json
{
    "station_id": "musikita",
    "type": "playlist",
    "files": [
        "/path/to/song1.mp3",
        "/path/to/song2.mp3"
    ]
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `station_id` | string | ✅ | Target station ID |
| `type` | string | ✅ | `"playlist"` or `"insert"` |
| `mode` | string | ❌ | `"append"` (default) or `"replace"` |
| `files` | array[string] | ✅ | Absolute paths to audio files on server |

### Mode: Append vs Replace

| Mode | Behavior | Use Case |
|------|----------|----------|
| `"append"` (default) | Add files to the **end** of the queue. | Growing the playlist |
| `"replace"` | **Replace the entire remaining queue** with new files. Currently playing track is **NOT interrupted**. | Updating full schedule/playlist |

### Queue Priority Logic

| Type | Priority | Crossfade | Use Case |
|------|-----------|-----------|----------|
| `insert` | 🥇 Highest | ❌ No crossfade | Bumpers, jingles, breaking news, ads |
| `playlist` | 🥈 Normal | ✅ 3s Crossfade | Songs, regular tracks |

**Rules:**
- `insert queue` MUST be empty before touching `playlist queue`.
- If both queues are empty → automatically plays `silent_5s.mp3` (looping silence).
- `mode: replace` only applies to `type: playlist`, not `insert`.

### Audio Format Support

| Format | Extensions |
|--------|------------|
| MP3 | `.mp3` |
| WAV | `.wav` |
| OGG | `.ogg` |
| FLAC | `.flac` |
| AAC | `.aac` |
| M4A | `.m4a` |
| WMA | `.wma` |

**Response `200 OK`:**

```json
{
    "status": "ok",
    "station_id": "musikita",
    "type": "playlist",
    "mode": "replace",
    "files": [
        "/path/to/song1.mp3",
        "/path/to/song2.mp3"
    ]
}
```

---

## 3. Create New Station

Dynamically creates a new station without restart.

```
POST /station/create
Content-Type: application/json
```

**Request Body:**

```json
{
    "station_id": "radio3"
}
```

**Response `200 OK`:**

```json
{
    "status": "ok",
    "station_id": "radio3",
    "output_dir": "/var/www/streamer/output/radio3"
}
```

---

## 4. Remove Station

Stops and removes a station.

```
POST /station/remove
Content-Type: application/json
```

**Request Body:**

```json
{
    "station_id": "radio3"
}
```

---

## 5. Remove Specific File from Queue

Removes a single file from the queue by filename (matches basename or full path).

```
POST /queue/remove
Content-Type: application/json
```

**Request Body:**

```json
{
    "station_id": "musikita",
    "type": "playlist",
    "filename": "Song A.mp3"
}
```

---

## 6. Clear Queue

Empties a station's queue.

```
POST /queue/clear
Content-Type: application/json
```

**Request Body:**

```json
{
    "station_id": "musikita",
    "type": "all"
}
```

| Field | Description |
|-------|-------------|
| `type` | `"insert"`, `"playlist"`, or `"all"` |

---

## 7. Station Configuration (Random / Loop / Unique)

Get or set station playback configuration.

```
GET  /station/config?station_id=xxx   # View config
POST /station/config                   # Update config
```

### POST (Update Config)

```json
{
    "station_id": "musikita",
    "config": {
        "random": true,
        "loop": false,
        "unique": true,
        "aac64": false,
        "aac96": false,
        "aac128": true,
        "opus32": false,
        "opus64": false,
        "opus96": true
    }
}
```

---

## 8. HLS Streaming

### 8.1 Master Playlist (Auto Bitrate)

```
GET /hls/{station_id}/master.m3u8
```

### 8.2 Variant Playlist (Per Bitrate)

```
GET /hls/{station_id}/{bitrate}/index.m3u8
```

| Bitrate | Path |
|---------|------|
| 64 kbps | `64k/index.m3u8` |
| 96 kbps | `96k/index.m3u8` |
| 128 kbps | `128k/index.m3u8` |

---

## 9. Traditional MP3 Streaming (Continuous)

Provides a legacy-compatible, low-latency continuous MP3 stream (Icecast-style).

```
GET /stream/{station_id}.mp3
```

| Feature | Description |
|---------|-------------|
| **Latency** | Low (1-3 seconds) |
| **Compatibility** | Winamp, VLC, Windows Media Player, Mobile Apps |
| **CDN** | ⚠️ Cannot be cached by standard CDNs (e.g. Cloudflare) |
| **Bandwidth** | Consumes origin bandwidth directly for every listener |

---

## 10. Health Check

```
GET /health
```

**Response `200 OK`:**
```json
{"status":"ok","app":"Go Audio Broadcaster","version":"1.0.0","stations":2}
```

---

## 11. CORS Policy

All endpoints allow access from any origin:
- `Access-Control-Allow-Origin: *`
- `Access-Control-Allow-Methods: GET, POST, OPTIONS`

---

## 12. Integration Examples

### HTML5 Player (HLS.js)

```html
<script src="https://cdn.jsdelivr.net/npm/hls.js@latest"></script>
<audio id="audio" controls></audio>
<script>
    const hls = new Hls();
    hls.loadSource('http://localhost:8080/hls/musikita/master.m3u8');
    hls.attachMedia(document.getElementById('audio'));
</script>
```

---

## 13. Audio Processing Pipeline

```
Request ──> Inject Queue
                  │
                  ▼
          ┌── Insert? ──> Concat (no crossfade)
          │
          ▼  Playlist
    ┌─ Prev=Silent? ──> Single file
    │
    │  Prev=Song
    ▼
    acrossfade=d=3 ──> loudnorm (EBU R128) ──> HLS Segmentation
                                                      │
                                                      ▼
                                              Rolling 10 segments
                                              seg_01.ts .. seg_10.ts
```

---

## 14. Security Note

**Important:** This API is unauthenticated by default. If you bind the server to `0.0.0.0` or expose port 8080 to the public internet, anyone can manage your stations. 

**Recommended Architecture:** Use a middleware/backend application (PHP, Node.js, etc.) on the same server to act as a bridge. Your main application should call the Streamer API via `localhost`, while exposing its own authenticated endpoints to the users.

Always use a firewall or a reverse proxy with authentication to secure your production environment.

---

*Generated by Go Audio Broadcaster v1.0.0*
