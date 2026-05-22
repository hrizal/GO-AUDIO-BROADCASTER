package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"strconv"

	"github.com/hrizal/GO-AUDIO-BROADCASTER/encoder"
	"github.com/hrizal/GO-AUDIO-BROADCASTER/station"
	"github.com/hrizal/GO-AUDIO-BROADCASTER/types"
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
	mux.HandleFunc("/mixer/buffer", h.corsMiddleware(h.handleMixerBuffer))
	mux.HandleFunc("/webrtc/offer", h.corsMiddleware(h.handleWebRTCOffer))
	mux.HandleFunc("/webrtc/publish", h.corsMiddleware(h.handleWebRTCPublish))
	mux.HandleFunc("/mixer/restart", h.corsMiddleware(h.handleMixerRestart))
	mux.HandleFunc("/mixer/skip", h.corsMiddleware(h.handleMixerSkip))
	mux.HandleFunc("/breaking", h.corsMiddleware(h.handleBreaking))

	// Manual Mixer Control routes
	mux.HandleFunc("/mixer/mode", h.corsMiddleware(h.handleMixerMode))
	mux.HandleFunc("/mixer/standby", h.corsMiddleware(h.handleMixerStandby))
	mux.HandleFunc("/mixer/play", h.corsMiddleware(h.handleMixerPlay))
	mux.HandleFunc("/mixer/mixing", h.corsMiddleware(h.handleMixerMixing))
	mux.HandleFunc("/mixer/pause", h.corsMiddleware(h.handleMixerPause))
	mux.HandleFunc("/mixer/rewind", h.corsMiddleware(h.handleMixerRewind))

	// Continuous low-latency streaming
	mux.HandleFunc("/stream/", h.corsMiddleware(h.handleStream))

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
		Duration  float64 `json:"duration"` // Default is 0 (instant) if not provided
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

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in auto mode, instruction ignored",
		})
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

func (h *Handler) handleMixerBuffer(w http.ResponseWriter, r *http.Request) {
	stationID := r.URL.Query().Get("id")
	chunksStr := r.URL.Query().Get("chunks")
	
	var chunks int
	var err error
	if stationID == "" || chunksStr == "" {
		if r.Method == http.MethodPost {
			var req struct {
				StationID string `json:"station_id"`
				Chunks    int    `json:"chunks"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				stationID = req.StationID
				chunks = req.Chunks
			}
		}
	} else {
		chunks, err = strconv.Atoi(chunksStr)
		if err != nil {
			http.Error(w, "Invalid chunks parameter", http.StatusBadRequest)
			return
		}
	}

	if stationID == "" || chunks <= 0 {
		http.Error(w, "Missing or invalid station_id or chunks parameter", http.StatusBadRequest)
		return
	}

	st, exists := h.manager.GetStation(stationID)
	if !exists {
		http.Error(w, "Station not found", http.StatusNotFound)
		return
	}

	if st.Encoder == nil || st.Encoder.Broadcaster == nil {
		http.Error(w, "Encoder or Broadcaster not active", http.StatusServiceUnavailable)
		return
	}

	// Dynamic, real-time capacity adjustment!
	st.Encoder.Broadcaster.SetCapacity(chunks)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": stationID,
		"chunks":     chunks,
		"kb":         chunks * 4,
		"seconds":    (chunks * 4 * 1024 * 8) / 64000,
	})
}

func (h *Handler) handleWebRTCOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stationID := r.URL.Query().Get("id")
	var req struct {
		StationID string `json:"station_id"`
		SDP       string `json:"sdp"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.StationID != "" {
		stationID = req.StationID
	}

	if stationID == "" || req.SDP == "" {
		http.Error(w, "Missing station_id or sdp", http.StatusBadRequest)
		return
	}

	st, exists := h.manager.GetStation(stationID)
	if !exists {
		http.Error(w, "Station not found", http.StatusNotFound)
		return
	}

	if st.Encoder == nil || st.Encoder.WebRTCBroadcaster == nil {
		http.Error(w, "WebRTC audio streaming is not active/enabled on this station", http.StatusServiceUnavailable)
		return
	}

	sdpAnswer, err := st.Encoder.WebRTCBroadcaster.HandleOffer(req.SDP)
	if err != nil {
		http.Error(w, "WebRTC SDP negotiation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": stationID,
		"sdp":        sdpAnswer,
	})
}

func (h *Handler) handleWebRTCPublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stationID := r.URL.Query().Get("id")
	token := r.URL.Query().Get("token")

	var req struct {
		StationID string `json:"station_id"`
		Token     string `json:"token"`
		SDP       string `json:"sdp"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		if req.StationID != "" {
			stationID = req.StationID
		}
		if req.Token != "" {
			token = req.Token
		}
	}

	if req.SDP == "" {
		bodyBytes, _ := io.ReadAll(r.Body)
		req.SDP = string(bodyBytes)
	}

	if stationID == "" || req.SDP == "" {
		http.Error(w, "Missing station_id or sdp", http.StatusBadRequest)
		return
	}

	st, exists := h.manager.GetStation(stationID)
	if !exists {
		http.Error(w, "Station not found", http.StatusNotFound)
		return
	}

	// SECURE PROTECTION: Validate the token configured in the station cfg
	configuredToken := st.Station.Config.WebRTCIngressToken
	if configuredToken == "" {
		http.Error(w, "WebRTC Ingress (publishing) is disabled on this station (no webrtc_ingress_token configured)", http.StatusForbidden)
		return
	}

	if token != configuredToken {
		http.Error(w, "Invalid security token", http.StatusUnauthorized)
		return
	}

	if st.Encoder == nil || st.Encoder.WebRTCBroadcaster == nil {
		http.Error(w, "WebRTC audio engine is not active on this station", http.StatusServiceUnavailable)
		return
	}

	sdpAnswer, err := st.Encoder.WebRTCBroadcaster.HandleIngressOffer(req.SDP, st.Encoder.Mixer)
	if err != nil {
		http.Error(w, "WebRTC Ingress SDP negotiation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": stationID,
		"sdp":        sdpAnswer,
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
		http.Error(w, `{"error": "Format JSON tidak valid. `+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, `{"error": "Parameter 'station_id' wajib diisi."}`, http.StatusBadRequest)
		return
	}

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err == nil && isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in manual mode, auto inject instruction ignored",
		})
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

	// Cek apakah channel sedang aktif jika ingin inject lagu
	if req.File != "" && !req.Force {
		statusList, err := h.manager.GetMixerStatus(req.StationID)
		if err == nil && targetChannelID >= 0 && targetChannelID < len(statusList) {
			if statusList[targetChannelID].Active {
				http.Error(w, `{"error": "Eh tunggu! Channel `+strconv.Itoa(targetChannelID)+` sedang memutar sesuatu ('`+statusList[targetChannelID].Label+`'). Jika kamu yakin ingin menimpanya, tambahkan parameter \"force\": true di dalam JSON."}`, http.StatusConflict)
				return
			}
		}
	}

	// Cek apakah file ada jika file dikirim
	if req.File != "" {
		if _, err := os.Stat(req.File); os.IsNotExist(err) {
			http.Error(w, `{"error": "File tidak ditemukan: `+req.File+`"}`, http.StatusBadRequest)
			return
		}
	}

	// Jalankan Smart Inject
	go func() {
		// 1. Eksekusi Fade In & Injeksi Lagu
		if req.File != "" {
			// Jika lagu akan diinjeksi, set volumenya ke 0 dulu secara instan agar senyap
			h.manager.SetMixerVolume(req.StationID, targetChannelID, 0.0, 0.0)
			
			// Injeksi lagu secara instan
			h.manager.PlayInstant(req.StationID, req.File, targetChannelID)
		}

		// Peta untuk menyimpan volume asli sebelum diubah
		type volData struct {
			origVol float64
			token   int64
		}
		channelData := make(map[int]*volData)

		// 2. Eksekusi Perubahan Volume (Smart Mixing)
		if volumes != nil {
			var wg sync.WaitGroup
			
			for chStr, volPct := range volumes {
				chID, err := strconv.Atoi(chStr)
				if err != nil {
					continue
				}
				
				origVol := h.manager.GetChannelVolume(req.StationID, chID)
				// Jika volume sedang diduck (kurang dari 1.0), asumsikan aslinya 1.0
				// karena itu pasti sedang diduck oleh insert sebelumnya yang ditimpa
				if origVol < 1.0 {
					origVol = 1.0
				}
				
				cd := &volData{origVol: origVol}
				channelData[chID] = cd

				targetVol := volPct / 100.0
				if targetVol > 1.0 { targetVol = 1.0 }
				if targetVol < 0.0 { targetVol = 0.0 }

				if req.File != "" && chID != targetChannelID {
					wg.Add(1)
					go func(cid int, tvol float64, dur float64, d *volData) {
						defer wg.Done()
						time.Sleep(1 * time.Second)
						token, _ := h.manager.SetMixerVolume(req.StationID, cid, tvol, dur)
						d.token = token
					}(chID, targetVol, fadeDur, cd)
				} else {
					token, _ := h.manager.SetMixerVolume(req.StationID, chID, targetVol, fadeDur)
					cd.token = token
				}
			}
			
			// Tunggu semua SetMixerVolume selesai (maks 1 detik) agar token terkumpul
			wg.Wait()
		}

		// 3. Auto-Restore Volume (Jika ada file yang diputar)
		if req.File != "" && volumes != nil {
			dur := encoder.GetAudioDuration(req.File)
			if dur > 1.0 {
				// Kurangi 1 detik karena kita sudah menunggu 1 detik saat fade out Bumbu Rahasia
				time.Sleep(time.Duration((dur - 1.0) * float64(time.Second)))
				
				// Kembalikan volume ke aslinya (HANYA jika token cocok, artinya tidak ditimpa insert lain)
				for chID, cd := range channelData {
					h.manager.RestoreMixerVolume(req.StationID, chID, cd.origVol, fadeDur, cd.token)
				}
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"message": "Smart Inject perintah dieksekusi",
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

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err == nil && isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in manual mode, skip instruction ignored",
		})
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

func (h *Handler) handleMixerMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
		Mode      string `json:"mode"` // "auto" or "manual"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.StationID == "" {
		http.Error(w, "station_id is required", http.StatusBadRequest)
		return
	}
	if req.Mode != "auto" && req.Mode != "manual" {
		http.Error(w, "mode must be 'auto' or 'manual'", http.StatusBadRequest)
		return
	}

	if err := h.manager.SetMixerMode(req.StationID, req.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"station_id": req.StationID,
		"mode":       req.Mode,
	})
}

func (h *Handler) handleMixerStandby(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
		Channel   int    `json:"channel"`
		File      string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in auto mode, instruction ignored",
		})
		return
	}

	if err := h.manager.SetMixerStandby(req.StationID, req.Channel, req.File); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) handleMixerPlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string  `json:"station_id"`
		Channel   int     `json:"channel"`
		Volume    float64 `json:"volume"`
		Duration  float64 `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in auto mode, instruction ignored",
		})
		return
	}

	if err := h.manager.PlayMixerChannel(req.StationID, req.Channel, req.Volume, req.Duration); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) handleMixerMixing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID   string  `json:"station_id"`
		UpChannel   int     `json:"up_channel"`
		UpVolume    float64 `json:"up_volume"`
		DownChannel int     `json:"down_channel"`
		DownVolume  float64 `json:"down_volume"`
		Duration    float64 `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in auto mode, instruction ignored",
		})
		return
	}

	if err := h.manager.MixChannels(req.StationID, req.UpChannel, req.UpVolume, req.DownChannel, req.DownVolume, req.Duration); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) handleMixerPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string `json:"station_id"`
		Channel   int    `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in auto mode, instruction ignored",
		})
		return
	}

	if err := h.manager.PauseMixerChannel(req.StationID, req.Channel); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) handleMixerRewind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		StationID string  `json:"station_id"`
		Channel   int     `json:"channel"`
		Seconds   float64 `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	isManual, err := h.manager.IsManualMode(req.StationID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !isManual {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ignored",
			"message": "Station is in auto mode, instruction ignored",
		})
		return
	}

	if err := h.manager.RewindMixerChannel(req.StationID, req.Channel, req.Seconds); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	// Parse station ID from URL. The path format is /stream/{station_id}.mp3
	path := r.URL.Path
	if !strings.HasPrefix(path, "/stream/") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	
	stationFile := strings.TrimPrefix(path, "/stream/")
	stationID := strings.TrimSuffix(stationFile, ".mp3")
	
	if stationID == "" {
		http.Error(w, "station_id is required", http.StatusBadRequest)
		return
	}
	
	st, exists := h.manager.GetStation(stationID)
	if !exists {
		http.Error(w, "Station not found", http.StatusNotFound)
		return
	}
	
	if !st.Station.Config.MP3 {
		http.Error(w, "MP3 streaming is disabled in station configuration. Please enable it by setting mp3=true in station.cfg and restarting the streamer service.", http.StatusForbidden)
		return
	}
	
	if st.Encoder == nil || st.Encoder.Broadcaster == nil {
		http.Error(w, "Streaming not available", http.StatusServiceUnavailable)
		return
	}
	
	// Add listener to broadcaster
	ch := st.Encoder.Broadcaster.AddListener()
	defer st.Encoder.Broadcaster.RemoveListener(ch)
	
	// Set response headers for continuous MP3 stream
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	ctx := r.Context()
	
	for {
		select {
		case <-ctx.Done():
			return
		case data, open := <-ch:
			if !open {
				return
			}
			_, err := w.Write(data)
			if err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}
