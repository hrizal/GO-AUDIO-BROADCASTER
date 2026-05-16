package station

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/streamer/encoder"
	"github.com/streamer/types"
)

// Manager coordinates all station runners
type Manager struct {
	stations   map[string]*StationRunner
	mu         sync.RWMutex
	silentDir  string
	outputDir  string
	configPath string // path to station.cfg for reload
}

// NewManager creates a new station manager
func NewManager(silentDir, outputDir string) *Manager {
	return &Manager{
		stations:  make(map[string]*StationRunner),
		silentDir: silentDir,
		outputDir: outputDir,
	}
}

// SetConfigPath stores the config file path for later reloads
func (m *Manager) SetConfigPath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configPath = path
}

// ReloadStationFromConfig re-reads station.cfg and applies config for one station.
func (m *Manager) ReloadStationFromConfig(stationID string) error {
	m.mu.RLock()
	configPath := m.configPath
	m.mu.RUnlock()

	if configPath == "" {
		return fmt.Errorf("config path not set")
	}

	entries, err := loadStationConfigs(configPath)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	var found *types.StationConfigEntry
	for _, e := range entries {
		if e.ID == stationID {
			found = &e
			break
		}
	}
	if found == nil {
		return fmt.Errorf("station %s not found in config file", stationID)
	}

	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station %s not running", stationID)
	}

	runner.Station.Lock()
	runner.Station.Config = found.Config
	runner.Station.Unlock()
	log.Printf("[Manager] Station %s config reloaded: random=%v loop=%v unique=%v aac64=%v aac96=%v aac128=%v opus32=%v opus64=%v opus96=%v hls_time=%v",
		stationID, found.Config.Random, found.Config.Loop, found.Config.Unique,
		found.Config.AAC64, found.Config.AAC96, found.Config.AAC128,
		found.Config.Opus32, found.Config.Opus64, found.Config.Opus96, found.Config.HlsTime)

	if found.OutputDir != "" && found.OutputDir != runner.Station.OutputDir {
		log.Printf("[Manager] Station %s output dir changed (%s). Restart required.",
			stationID, found.OutputDir)
	}
	return nil
}

func loadStationConfigs(path string) ([]types.StationConfigEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []types.StationConfigEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		stationID := parts[0]
		valid := true
		for _, ch := range stationID {
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
				valid = false
				break
			}
		}
		if !valid {
			log.Printf("[Config] Warning: skipping invalid station ID %q", line)
			continue
		}

		cfg := types.DefaultPlaybackConfig()
		var outputDir string
		var entryPlaylist string
		for _, part := range parts[1:] {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(kv[0]))
			val := strings.TrimSpace(kv[1])
			valLower := strings.ToLower(val)
			boolVal := valLower == "true" || valLower == "1" || valLower == "yes"

			switch key {
			case "random":
				cfg.Random = boolVal
			case "loop":
				cfg.Loop = boolVal
			case "unique":
				cfg.Unique = boolVal
			case "aac64":
				cfg.AAC64 = boolVal
			case "aac96":
				cfg.AAC96 = boolVal
			case "aac128":
				cfg.AAC128 = boolVal
			case "opus32":
				cfg.Opus32 = boolVal
			case "opus64":
				cfg.Opus64 = boolVal
			case "opus96":
				cfg.Opus96 = boolVal
			case "output":
				if filepath.IsAbs(val) {
					outputDir = val
				} else {
					if absVal, err := filepath.Abs(val); err == nil {
						outputDir = absVal
					} else {
						outputDir = val
					}
				}
			case "playlist":
				if filepath.IsAbs(val) {
					entryPlaylist = val
				} else {
					if absVal, err := filepath.Abs(val); err == nil {
						entryPlaylist = absVal
					} else {
						entryPlaylist = val
					}
				}
			case "hls_time":
				var t int
				if n, err := fmt.Sscanf(val, "%d", &t); err == nil && n == 1 {
					cfg.HlsTime = t
				}
			}
		}
		entries = append(entries, types.StationConfigEntry{
			ID:         stationID,
			Config:     cfg,
			OutputDir:  outputDir,
			PlaylistDir: entryPlaylist,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading %s: %w", path, err)
	}
	return entries, nil
}

func (m *Manager) CreateStation(id string) (*StationRunner, error) {
	return m.createStationWithDir(id, "", "")
}

func (m *Manager) CreateStationWithOutput(id string, outputDir string) (*StationRunner, error) {
	return m.createStationWithDir(id, outputDir, "")
}

// CreateStationFromEntry creates a station from a config entry, with auto playlist inject
func (m *Manager) CreateStationFromEntry(entry types.StationConfigEntry) (*StationRunner, error) {
	runner, err := m.createStationWithDir(entry.ID, entry.OutputDir, entry.PlaylistDir)
	if err != nil {
		return nil, err
	}
	// Apply config
	runner.Station.Lock()
	runner.Station.Config = entry.Config
	runner.Station.Unlock()
	return runner, nil
}

func (m *Manager) createStationWithDir(id string, outputDir string, playlistDir string) (*StationRunner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.stations[id]; exists {
		log.Printf("[Manager] Station %s already exists", id)
		return m.stations[id], nil
	}

	var station *types.Station
	if outputDir != "" {
		station = types.NewStationWithOutput(id, outputDir)
	} else {
		station = types.NewStation(id, m.outputDir)
	}

	runner := NewStationRunner(station, m.silentDir)
	if err := runner.InitStation(); err != nil {
		return nil, err
	}

	m.stations[id] = runner
	go runner.Run()

	// Auto-inject playlist if configured
	if playlistDir != "" {
		var allFiles []string
		extensions := []string{"*.mp3", "*.wav", "*.ogg", "*.flac", "*.aac", "*.m4a", "*.wma", "*.mp4", "*.mkv", "*.avi"}
		for _, ext := range extensions {
			files, err := filepath.Glob(filepath.Join(playlistDir, ext))
			if err == nil {
				allFiles = append(allFiles, files...)
			}
		}

		if len(allFiles) > 0 {
			runner.QueueMgr.ReplacePlaylist(allFiles)
			log.Printf("[Manager] Station %s auto-injected %d files from %s", id, len(allFiles), playlistDir)
		} else {
			log.Printf("[Manager] Station %s: no audio files found in %s", id, playlistDir)
		}
	}

	log.Printf("[Manager] Station %s created and started", id)
	return runner, nil
}

func (m *Manager) GetStation(id string) (*StationRunner, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	runner, exists := m.stations[id]
	return runner, exists
}

func (m *Manager) RemoveStation(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	runner, exists := m.stations[id]
	if !exists {
		return
	}
	runner.Station.Stop()
	delete(m.stations, id)
	log.Printf("[Manager] Station %s removed", id)
}

func (m *Manager) SnapshotAll() []types.QueueSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshots := make([]types.QueueSnapshot, 0, len(m.stations))
	for _, runner := range m.stations {
		insertLen, playlistLen := runner.QueueMgr.QueueLengths()
		runner.Station.RLock()
		snapshots = append(snapshots, types.QueueSnapshot{
			StationID:        runner.Station.ID,
			Status:           string(runner.Station.Status),
			PreviousTrack:    runner.Station.PreviousTrack,
			CurrentTrack:     runner.Station.CurrentTrack,
			NextTrack:        runner.Station.NextTrack,
			Config:           runner.Station.Config,
			InsertQueueLen:   insertLen,
			PlaylistQueueLen: playlistLen,
		})
		runner.Station.RUnlock()
	}
	return snapshots
}

func (m *Manager) InjectFiles(stationID string, qtype string, files []string, mode string) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}

	switch qtype {
	case "insert":
		runner.QueueMgr.PushInsert(files)
	case "playlist":
		if mode == "replace" {
			runner.QueueMgr.ReplacePlaylist(files)
		} else {
			runner.QueueMgr.PushPlaylist(files)
		}
	default:
		return fmt.Errorf("invalid queue type: %s", qtype)
	}
	return nil
}

func (m *Manager) ClearQueue(stationID string, queueType string) (int, error) {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return 0, fmt.Errorf("station not found: %s", stationID)
	}
	removed := runner.QueueMgr.ClearQueue(queueType)
	return removed, nil
}

func (m *Manager) RemoveFromQueue(stationID string, queueType string, filename string) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}
	if ok := runner.QueueMgr.RemoveFromQueue(queueType, filename); !ok {
		return fmt.Errorf("file not found in %s queue: %s", queueType, filename)
	}
	return nil
}

func (m *Manager) SetConfig(stationID string, cfg types.PlaybackConfig) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}
	runner.Station.Lock()
	runner.Station.Config = cfg
	runner.Station.Unlock()
	log.Printf("[Manager] Station %s config updated: random=%v loop=%v unique=%v aac64=%v aac96=%v aac128=%v opus32=%v opus64=%v opus96=%v hls_time=%v",
		stationID, cfg.Random, cfg.Loop, cfg.Unique,
		cfg.AAC64, cfg.AAC96, cfg.AAC128,
		cfg.Opus32, cfg.Opus64, cfg.Opus96, cfg.HlsTime)
	return nil
}

func (m *Manager) StationCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.stations)
}

func (m *Manager) ListStations() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.stations))
	for id := range m.stations {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, runner := range m.stations {
		runner.Station.Stop()
		delete(m.stations, id)
	}
	log.Printf("[Manager] All stations stopped")
}

func (m *Manager) Skip(stationID string) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}
	runner.Station.Skip()
	return nil
}

func (m *Manager) RestartCurrent(stationID string) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}

	runner.Station.RLock()
	currentFile := runner.Station.CurrentFile
	runner.Station.RUnlock()

	if currentFile == "" || strings.HasSuffix(currentFile, "silent_5s.mp3") {
		return fmt.Errorf("no current track to restart")
	}

	// Put it back to the front of the queue
	runner.QueueMgr.PushPlaylistFront([]string{currentFile})
	
	// Skip to switch (since it's at the front, it will replay)
	runner.Station.Skip()
	return nil
}

func (m *Manager) SetMixerVolume(stationID string, channelID int, volume float64) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}
	if runner.Encoder.Mixer == nil {
		return fmt.Errorf("mixer not initialized for station %s", stationID)
	}
	if channelID < 0 || channelID >= len(runner.Encoder.Mixer.Channels) {
		return fmt.Errorf("invalid channel ID: %d", channelID)
	}

	runner.Encoder.Mixer.Channels[channelID].SetVolume(volume)
	return nil
}

func (m *Manager) SetMixerMute(stationID string, channelID int, mute bool) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}
	if runner.Encoder.Mixer == nil {
		return fmt.Errorf("mixer not initialized for station %s", stationID)
	}
	if channelID < 0 || channelID >= len(runner.Encoder.Mixer.Channels) {
		return fmt.Errorf("invalid channel ID: %d", channelID)
	}

	runner.Encoder.Mixer.Channels[channelID].SetMute(mute)
	return nil
}

func (m *Manager) GetMixerStatus(stationID string) ([]encoder.ChannelStatus, error) {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("station not found: %s", stationID)
	}
	if runner.Encoder.Mixer == nil {
		return nil, fmt.Errorf("mixer not initialized for station %s", stationID)
	}

	return runner.Encoder.Mixer.GetStatus(), nil
}

func (m *Manager) PlayInstant(stationID string, file string, channelID int) error {
	m.mu.RLock()
	runner, exists := m.stations[stationID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("station not found: %s", stationID)
	}
	runner.Encoder.PlayInstant(file, channelID)
	return nil
}
