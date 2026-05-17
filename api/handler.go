package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/streamer/encoder"
	"github.com/streamer/station"
	"github.com/streamer/types"
)

// Handler manages HTTP endpoints
type Handler struct {
	manager  *station.Manager
	serveDir string // directory to serve HLS output files
}

// NewHandler creates a new API handler
func NewHandler(manager *station.Manager, serveDir string) *Handler {
	return &Handler{
		manager:  manager,
		serveDir: serveDir,
	}
}

// RegisterRoutes registers all HTTP routes
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/status", h.corsMiddleware(h.handleStatus))
	mux.HandleFunc("/inject", h.corsMiddleware(h.handleInject))
	mux.HandleFunc("/station/create", h.corsMiddleware(h.handleCreateStation))
	mux.HandleFunc("/station/remove", h.corsMiddleware(h.handleRemoveStation))

	// Queue management
	mux.HandleFunc("/queue/clear", h.corsMiddleware(h.handleQueueClear))
	mux.HandleFunc("/queue/remove", h.corsMiddleware(h.handleQueueRemove))

	// Station config
	mux.HandleFunc("/station/config", h.corsMiddleware(h.handleStationConfig))

	// Reload config from file
	mux.HandleFunc("/station/reload", h.corsMiddleware(h.handleStationReload))

	// Mixer control
	mux.HandleFunc("/mixer/status", h.corsMiddleware(h.handleMixerStatus))
	mux.HandleFunc("/mixer/volume", h.corsMiddleware(h.handleMixerVolume))
	mux.HandleFunc("/mixer/mute", h.corsMiddleware(h.handleMixerMute))
	mux.HandleFunc("/mixer/restart", h.corsMiddleware(h.handleMixerRestart))
	mux.HandleFunc("/mixer/skip", h.corsMiddleware(h.handleMixerSkip))
	mux.HandleFunc("/breaking", h.corsMiddleware(h.handleBreaking))

	// Serve HLS output files for clients
	hlsServer := http.StripPrefix("/hls/", http.FileServer(http.Dir(h.serveDir)))
	mux.Handle("/hls/", h.corsMiddleware(hlsServer.ServeHTTP))
}

// RegisterPort80Routes registers routes specifically for port 80 (Shortcuts)
func (h *Handler) RegisterPort80Routes(mux *http.ServeMux) {
	mux.HandleFunc("/", h.corsMiddleware(h.handlePort80))
	
	// Also serve HLS on port 80 so redirects work
	hlsServer := http.StripPrefix("/hls/", http.FileServer(http.Dir(h.serveDir)))
	mux.Handle("/hls/", h.corsMiddleware(hlsServer.ServeHTTP))
}

// handlePort80 redirects root requests to the master playlist for each station
func (h *Handler) handlePort80(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	
	// If it's just "/", show a simple text info or list of stations
	if path == "" {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("Go Audio Broadcaster - Active Stations:\n\n"))
		for _, id := range h.manager.ListStations() {
			w.Write([]byte("- http://" + r.Host + "/" + id + "/\n"))
		}
		return
	}

	// If it matches a station ID, redirect or serve its master.m3u8
	// Format: http://ip/radio1/ -> /hls/radio1/master.m3u8
	parts := strings.Split(path, "/")
	stationID := parts[0]
	
	// Check if station exists
	if _, exists := h.manager.GetStation(stationID); exists {
		// Serve the master playlist directly or redirect
		// Redirecting is safer so the client gets the right relative paths for segments
		http.Redirect(w, r, "/hls/"+stationID+"/master.m3u8", http.StatusFound)
		return
	}

	http.NotFound(w, r)
}

// corsMiddleware adds CORS headers + cache control for HLS
func (h *Handler) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// Prevent caching for HLS playlists (live stream)
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		} else if strings.HasSuffix(r.URL.Path, ".ts") || strings.HasSuffix(r.URL.Path, ".mp4") {
			w.Header().Set("Cache-Control", "no-cache")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// handleStatus returns JSON status of a single station.
//   GET /status?station_id=xxx
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stationID := r.URL.Query().Get("station_id")
	if stationID == "" {
		http.Error(w, "station_id query parameter required", http.StatusBadRequest)
		return
	}

	var snapshot *types.QueueSnapshot
	for _, s := range h.manager.SnapshotAll() {
		if s.StationID == stationID {
			snapshot = &s
			break
		}
	}
	if snapshot == nil {
		http.Error(w, "station not found: "+stationID, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"status":   snapshot,
	})
}

// handleInject adds files to a station's queue
func (h *Handler) handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req types.InjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Normalize mode
	mode := req.Mode
	if mode == "" {
		mode = "append"
	}
	if mode != "append" && mode != "replace" {
		http.Error(w, "mode must be 'append' or 'replace'", http.StatusBadRequest)
		return
	}
	if mode == "replace" && req.Type == "insert" {
		http.Error(w, "replace mode is not supported for insert type", http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, "station_id is required", http.StatusBadRequest)
		return
	}
	if req.Type != "playlist" && req.Type != "insert" {
		http.Error(w, "type must be 'playlist' or 'insert'", http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		http.Error(w, "files array cannot be empty", http.StatusBadRequest)
		return
	}

	// Validate files exist/accessible
	for _, f := range req.Files {
		// If it's a URL, skip local file check
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			continue
		}
		if _, err := os.Stat(f); os.IsNotExist(err) {
			http.Error(w, "File not found: "+f, http.StatusBadRequest)
			return
		}
	}

	// Validate files are audio
	for _, f := range req.Files {
		// If it's a URL, assume FFmpeg can handle it
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			continue
		}
		
		// Basic extension check
		ext := filepath.Ext(f)
		switch ext {
		case ".mp3", ".wav", ".ogg", ".flac", ".aac", ".m4a", ".wma":
			// supported
		default:
			http.Error(w, "Unsupported audio format: "+f, http.StatusBadRequest)
			return
		}
	}

	if err := h.manager.InjectFiles(req.StationID, req.Type, req.Files, mode); err != nil {
		http.Error(w, "Injection failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update crossfade config if provided
	if req.Crossfade != nil && req.Type == "playlist" {
		if st, ok := h.manager.GetStation(req.StationID); ok {
			newCfg := st.Station.Config
			newCfg.Crossfade = *req.Crossfade
			h.manager.SetConfig(req.StationID, newCfg)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": req.StationID,
		"type":       req.Type,
		"mode":       mode,
		"files":      req.Files,
	})

	log.Printf("[API] Injected %d files into station %s (type: %s, mode: %s)", len(req.Files), req.StationID, req.Type, mode)
}

// handleCreateStation creates a new station
func (h *Handler) handleCreateStation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, "station_id is required", http.StatusBadRequest)
		return
	}

	runner, err := h.manager.CreateStation(req.StationID)
	if err != nil {
		http.Error(w, "Failed to create station: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": runner.Station.ID,
		"output_dir": runner.Station.OutputDir,
	})

	log.Printf("[API] Station %s created", req.StationID)
}

// handleRemoveStation removes a station
func (h *Handler) handleRemoveStation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.manager.RemoveStation(req.StationID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": req.StationID,
	})

	log.Printf("[API] Station %s removed", req.StationID)
}

// handleQueueClear clears a station's queue
func (h *Handler) handleQueueClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
		Type      string `json:"type"` // "insert", "playlist", or "all"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, "station_id required", http.StatusBadRequest)
		return
	}
	if req.Type != "insert" && req.Type != "playlist" && req.Type != "all" {
		http.Error(w, "type must be 'insert', 'playlist', or 'all'", http.StatusBadRequest)
		return
	}

	removed, err := h.manager.ClearQueue(req.StationID, req.Type)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": req.StationID,
		"type":       req.Type,
		"removed":    removed,
	})

	log.Printf("[API] Queue %s cleared for %s (%d removed)", req.Type, req.StationID, removed)
}

// handleQueueRemove removes a specific file from a station's queue
func (h *Handler) handleQueueRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
		Type      string `json:"type"`  // "insert" or "playlist"
		Filename  string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, "station_id required", http.StatusBadRequest)
		return
	}
	if req.Type != "insert" && req.Type != "playlist" {
		http.Error(w, "type must be 'insert' or 'playlist'", http.StatusBadRequest)
		return
	}
	if req.Filename == "" {
		http.Error(w, "filename required", http.StatusBadRequest)
		return
	}

	if err := h.manager.RemoveFromQueue(req.StationID, req.Type, req.Filename); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": req.StationID,
		"type":       req.Type,
		"filename":   req.Filename,
	})

	log.Printf("[API] Removed from %s queue [%s]: %s", req.StationID, req.Type, req.Filename)
}

// handleStationConfig gets or sets station playback config
func (h *Handler) handleStationConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		// GET /station/config?station_id=xxx
		stationID := r.URL.Query().Get("station_id")
		if stationID == "" {
			http.Error(w, "station_id query param required", http.StatusBadRequest)
			return
		}

		// Find station from snapshot
		for _, s := range h.manager.SnapshotAll() {
			if s.StationID == stationID {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"station_id": s.StationID,
					"config":     s.Config,
				})
				return
			}
		}
		http.Error(w, "station not found", http.StatusNotFound)

	case http.MethodPost:
		// POST /station/config (set)
		var req types.ConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.StationID == "" {
			http.Error(w, "station_id required", http.StatusBadRequest)
			return
		}

		if err := h.manager.SetConfig(req.StationID, req.Config); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":     "ok",
			"station_id": req.StationID,
			"config":     req.Config,
		})
		log.Printf("[API] Station %s config updated: random=%v loop=%v unique=%v aac64=%v aac96=%v aac128=%v opus32=%v opus64=%v opus96=%v",
			req.StationID, req.Config.Random, req.Config.Loop, req.Config.Unique,
			req.Config.AAC64, req.Config.AAC96, req.Config.AAC128,
			req.Config.Opus32, req.Config.Opus64, req.Config.Opus96)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleStationReload reloads station config from station.cfg
func (h *Handler) handleStationReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, "station_id required", http.StatusBadRequest)
		return
	}

	if err := h.manager.ReloadStationFromConfig(req.StationID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": req.StationID,
	})
	log.Printf("[API] Station %s config reloaded from file", req.StationID)
}

func (h *Handler) handleMixerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stationID := r.URL.Query().Get("station_id")
	if stationID == "" {
		http.Error(w, "station_id required", http.StatusBadRequest)
		return
	}

	status, err := h.manager.GetMixerStatus(stationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": stationID,
		"channels":   status,
	})
}

func (h *Handler) handleMixerVolume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string  `json:"station_id"`
		Channel   int     `json:"channel"`
		Volume    float64 `json:"volume"`
		Duration  float64 `json:"duration"` // optional duration
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if _, err := h.manager.SetMixerVolume(req.StationID, req.Channel, req.Volume, req.Duration); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) handleMixerMute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
		Channel   int    `json:"channel"`
		Mute      bool   `json:"mute"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.manager.SetMixerMute(req.StationID, req.Channel, req.Mute); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) handleBreaking(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string             `json:"station_id"`
		File      string             `json:"file"`
		Channel   *int               `json:"channel"`
		Crossfade float64            `json:"crossfade"`
		Volumes   map[string]float64 `json:"volumes"`
		Force     bool               `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "Invalid JSON format: `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, `{"error": "Parameter 'station_id' is required."}`, http.StatusBadRequest)
		return
	}

	// Default channel to 0 (voice/announcer channel) if omitted
	targetChannelID := 0
	if req.Channel != nil {
		targetChannelID = *req.Channel
	}

	// Default crossfade duration to 3.0 seconds if omitted or zero
	fadeDur := req.Crossfade
	if fadeDur <= 0.0 {
		fadeDur = 3.0
	}

	// Default volumes map to standard announcer ducking profile if omitted
	volumes := req.Volumes
	if volumes == nil {
		volumes = map[string]float64{
			"0": 100.0, // target announcer channel
			"1": 10.0,  // duck playlist channel 1
			"2": 10.0,  // duck playlist channel 2
		}
		// In case they set a non-zero channel, ensure it is set to 100
		if targetChannelID != 0 {
			chStr := strconv.Itoa(targetChannelID)
			volumes[chStr] = 100.0
		}
	}

	// Check if channel is active to prevent collision
	if req.File != "" && !req.Force {
		statusList, err := h.manager.GetMixerStatus(req.StationID)
		if err == nil && targetChannelID >= 0 && targetChannelID < len(statusList) {
			if statusList[targetChannelID].Active {
				http.Error(w, `{"error": "Wait! Channel `+strconv.Itoa(targetChannelID)+` is currently playing something ('`+statusList[targetChannelID].Label+`'). If you are sure you want to override it, add the parameter \"force\": true to your JSON payload."}`, http.StatusConflict)
				return
			}
		}
	}

	// Check if file exists
	if req.File != "" {
		if _, err := os.Stat(req.File); os.IsNotExist(err) {
			http.Error(w, `{"error": "File not found: `+req.File+`"}`, http.StatusBadRequest)
			return
		}
	}

	// Execute Smart Inject in background
	go func() {
		// 1. Instant fade-in and injection
		if req.File != "" {
			// Set volume to 0 instantly before playing to ensure smooth fade
			h.manager.SetMixerVolume(req.StationID, targetChannelID, 0.0, 0.0)
			
			h.manager.PlayInstant(req.StationID, req.File, targetChannelID)
		}

		// Map to hold original volumes before modification
		type volData struct {
			origVol float64
			token   int64
		}
		channelData := make(map[int]*volData)

		// 2. Execute volume modifications (Smart Ducking)
		if volumes != nil {
			var wg sync.WaitGroup
			
			for chStr, volPct := range volumes {
				chID, err := strconv.Atoi(chStr)
				if err != nil {
					continue
				}
				
				origVol := h.manager.GetChannelVolume(req.StationID, chID)
				// If volume is currently ducked (less than 1.0), assume baseline is 1.0
				// as it's likely being ducked by an overlapping insert
				if origVol < 1.0 {
					origVol = 1.0
				}
				
				cd := &volData{origVol: origVol}
				channelData[chID] = cd

				// Normalize 0-100 to 0.0-1.0
				targetVol := volPct / 100.0
				if targetVol > 1.0 { targetVol = 1.0 }
				if targetVol < 0.0 { targetVol = 0.0 }

				if req.File != "" && chID != targetChannelID {
					// Delay fade out on other channels by 1 second so the injected audio comes in first
					wg.Add(1)
					go func(cid int, tvol float64, dur float64, d *volData) {
						defer wg.Done()
						time.Sleep(1 * time.Second)
						token, _ := h.manager.SetMixerVolume(req.StationID, cid, tvol, dur)
						d.token = token
					}(chID, targetVol, fadeDur, cd)
				} else {
					// Fade in target channel immediately
					token, _ := h.manager.SetMixerVolume(req.StationID, chID, targetVol, fadeDur)
					cd.token = token
				}
			}
			
			// Wait for all SetMixerVolume calls (max 1 second) to gather tokens
			wg.Wait()
		}

		// 3. Auto-Restore Volume (if file duration is known)
		if req.File != "" && volumes != nil {
			dur := encoder.GetAudioDuration(req.File)
			if dur > 1.0 {
				// Subtract 1 second as we already waited 1 second during delayed fade-out
				time.Sleep(time.Duration((dur - 1.0) * float64(time.Second)))
				
				// Restore original volume (only if token matches, meaning no other insert took over)
				for chID, cd := range channelData {
					h.manager.RestoreMixerVolume(req.StationID, chID, cd.origVol, fadeDur, cd.token)
				}
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": "Smart inject command executed successfully",
	})
}

func (h *Handler) handleMixerRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.manager.RestartCurrent(req.StationID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"msg":    "Current track restart triggered",
	})
}

func (h *Handler) handleMixerSkip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.manager.Skip(req.StationID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"msg":    "Skip triggered",
	})
}
