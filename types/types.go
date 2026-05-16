package types

import "sync"

// StationStatus represents the current state of a station
type StationStatus string

const (
	StatusPlaying StationStatus = "playing"
	StatusSilent  StationStatus = "silent"
	StatusInsert  StationStatus = "insert"
)

// Bitrate quality levels for AAC
type BitrateLevel int

const (
	Bitrate64  BitrateLevel = 64
	Bitrate96  BitrateLevel = 96
	Bitrate128 BitrateLevel = 128
)

// OpusBitrate levels
type OpusBitrateLevel int

const (
	OpusBitrate32  OpusBitrateLevel = 32
	OpusBitrate64  OpusBitrateLevel = 64
	OpusBitrate96  OpusBitrateLevel = 96
	OpusBitrate128 OpusBitrateLevel = 128
)

func (b BitrateLevel) Label() string {
	switch b {
	case Bitrate64:
		return "64k"
	case Bitrate96:
		return "96k"
	case Bitrate128:
		return "128k"
	default:
		return "128k"
	}
}

func (b BitrateLevel) AudioChannels() string {
	switch b {
	case Bitrate64:
		return "mono"
	default:
		return "stereo"
	}
}

func (b OpusBitrateLevel) Label() string {
	switch b {
	case OpusBitrate32:
		return "32k"
	case OpusBitrate64:
		return "64k"
	case OpusBitrate96:
		return "96k"
	case OpusBitrate128:
		return "128k"
	default:
		return "64k"
	}
}

func (b OpusBitrateLevel) AudioChannels() string {
	switch b {
	case OpusBitrate32:
		return "mono"
	default:
		return "stereo"
	}
}

// PlaybackConfig holds playback behavior settings for a station
type PlaybackConfig struct {
	Random bool `json:"random"`
	Loop   bool `json:"loop"`
	Unique bool `json:"unique"`
	// Output variants
	AAC64  bool `json:"aac64"`
	AAC96  bool `json:"aac96"`
	AAC128 bool `json:"aac128"`
	Opus32  bool `json:"opus32"`
	Opus64  bool `json:"opus64"`
	Opus96  bool `json:"opus96"`
	Opus128 bool `json:"opus128"`
	MP3    bool `json:"mp3"`
	// HLS segment duration
	HlsTime     int    `json:"hls_time"`
	RTMP        string `json:"rtmp"`
	Logo        string `json:"logo"`
	VideoLoop   string `json:"video_loop"`
	DisplayText string `json:"display_text"`
	// Crossfade duration in seconds
	Crossfade int `json:"crossfade"`
}

func DefaultPlaybackConfig() PlaybackConfig {
	return PlaybackConfig{
		Random:    false,
		Loop:      true,
		Unique:    true,
		AAC64:     true,
		AAC96:     true,
		AAC128:    true,
		Opus32:    true,
		Opus64:    true,
		Opus96:    true,
		Opus128:   true,
		MP3:       true,
		HlsTime:   10,
		Crossfade: 3,
	}
}

// Station holds all runtime state for a single station
type Station struct {
	ID            string         `json:"id"`
	Status        StationStatus  `json:"status"`
	PreviousTrack string         `json:"previous_track"`
	CurrentTrack  string         `json:"current_track"`
	CurrentFile   string         `json:"-"`
	NextTrack     string         `json:"next_track"`
	Config        PlaybackConfig `json:"config"`
	InsertQueue   []string       `json:"-"`
	PlaylistQueue []string       `json:"-"`
	OriginalQueue []string       `json:"-"`
	PlayedSet     map[string]bool `json:"-"`
	mu            sync.RWMutex   `json:"-"`
	stopCh        chan struct{}  `json:"-"`
	skipCh        chan struct{}  `json:"-"`
	OutputDir     string         `json:"output_dir"`
	LogPrefix     string         `json:"-"`
}

func NewStation(id string, outputBase string) *Station {
	return &Station{
		ID:            id,
		Status:        StatusSilent,
		PreviousTrack: "",
		CurrentTrack:  "silent_5s.mp3",
		NextTrack:     "",
		Config:        DefaultPlaybackConfig(),
		InsertQueue:   make([]string, 0),
		PlaylistQueue: make([]string, 0),
		OriginalQueue: make([]string, 0),
		PlayedSet:     make(map[string]bool),
		stopCh:        make(chan struct{}),
		skipCh:        make(chan struct{}, 1),
		OutputDir:     outputBase + "/" + id,
		LogPrefix:     "[Station:" + id + "]",
	}
}

func NewStationWithOutput(id string, outputDir string) *Station {
	return &Station{
		ID:            id,
		Status:        StatusSilent,
		PreviousTrack: "",
		CurrentTrack:  "silent_5s.mp3",
		NextTrack:     "",
		Config:        DefaultPlaybackConfig(),
		InsertQueue:   make([]string, 0),
		PlaylistQueue: make([]string, 0),
		OriginalQueue: make([]string, 0),
		PlayedSet:     make(map[string]bool),
		stopCh:        make(chan struct{}),
		skipCh:        make(chan struct{}, 1),
		OutputDir:     outputDir,
		LogPrefix:     "[Station:" + id + "]",
	}
}

func (s *Station) Lock()       { s.mu.Lock() }
func (s *Station) Unlock()     { s.mu.Unlock() }
func (s *Station) RLock()      { s.mu.RLock() }
func (s *Station) RUnlock()    { s.mu.RUnlock() }
func (s *Station) StopChan() chan struct{} { return s.stopCh }
func (s *Station) SkipChan() chan struct{} { return s.skipCh }

func (s *Station) Skip() {
	select {
	case s.skipCh <- struct{}{}:
	default:
	}
}

func (s *Station) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

// QueueSnapshot for API responses
type QueueSnapshot struct {
	StationID        string         `json:"station_id"`
	Status           string         `json:"status"`
	PreviousTrack    string         `json:"previous_track"`
	CurrentTrack     string         `json:"current_track"`
	NextTrack        string         `json:"next_track"`
	Config           PlaybackConfig `json:"config"`
	InsertQueueLen   int            `json:"insert_queue_length"`
	PlaylistQueueLen int            `json:"playlist_queue_length"`
}

type ConfigRequest struct {
	StationID string         `json:"station_id"`
	Config    PlaybackConfig `json:"config"`
}

type InjectRequest struct {
	StationID string   `json:"station_id"`
	Type      string   `json:"type"`
	Mode      string   `json:"mode"`
	Files     []string `json:"files"`
}

// BitrateVariants holds directories for AAC (.ts) and Opus (.mp4)
type BitrateVariants struct {
	// AAC variants (mpegts container)
	AAC64  string
	AAC96  string
	AAC128 string
	// Opus variants (fmp4 container)
	Opus32 string
	Opus64  string
	Opus96  string
	Opus128 string
}

func NewBitrateVariants(stationDir string) BitrateVariants {
	return BitrateVariants{
		AAC64:  stationDir + "/aac/64k",
		AAC96:  stationDir + "/aac/96k",
		AAC128: stationDir + "/aac/128k",
		Opus32: stationDir + "/opus/32k",
		Opus64: stationDir + "/opus/64k",
		Opus96:  stationDir + "/opus/96k",
		Opus128: stationDir + "/opus/128k",
	}
}

func (bv BitrateVariants) AllAAC() []string {
	return []string{bv.AAC64, bv.AAC96, bv.AAC128}
}

func (bv BitrateVariants) AllOpus() []string {
	return []string{bv.Opus32, bv.Opus64, bv.Opus96, bv.Opus128}
}

func (bv BitrateVariants) All() []string {
	return append(bv.AllAAC(), bv.AllOpus()...)
}

type MasterPlaylistData struct {
	StationID string
	Variants  []VariantInfo
}

type VariantInfo struct {
	Bandwidth int
	Codec     string
	Path      string
}

type PlaylistData struct {
	StationID    string
	Bitrate      string
	SegmentPaths []string
	SequenceNum  int
	TargetDur    int
}

type StationConfigEntry struct {
	ID         string
	Config     PlaybackConfig
	OutputDir  string
	PlaylistDir string // auto-inject on startup
}
