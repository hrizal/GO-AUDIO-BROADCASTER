package encoder

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hrizal/GO-AUDIO-BROADCASTER/types"
)

const (
	NumSlots   = 30
	SegmentDur = 10.0
)

type AudioEngine struct {
	station   *types.Station
	variants  types.BitrateVariants
	mu        sync.Mutex
	prevFile  string
	tempDir   string
	ffmpegCmd *exec.Cmd
	ffmpegIn  io.WriteCloser
	started   bool
	Mixer       *AudioMixer
	nextCh      int // For alternating playlist channels (1 & 2)
	channelCmds map[int]*exec.Cmd
	// Live MP3 Stream support
	Broadcaster *Broadcaster
	streamCmd   *exec.Cmd
	rtmpCmd     *exec.Cmd
	// WebRTC Low Latency Stream support
	WebRTCBroadcaster *WebRTCBroadcaster
	webrtcCmd         *exec.Cmd
	instantMu   sync.Map // Map of channelID -> *sync.Mutex for queueing
}

func NewAudioEngine(station *types.Station, variants types.BitrateVariants) *AudioEngine {
	tempDir := filepath.Join(station.OutputDir, ".temp")
	os.MkdirAll(tempDir, 0755)
	return &AudioEngine{
		station:  station,
		variants: variants,
		tempDir:     tempDir,
		nextCh:      1, // Start with channel 1
		channelCmds: make(map[int]*exec.Cmd),
		Broadcaster: NewBroadcaster(),
	}
}

type Transition struct {
	PrevFile string
	NextFile string
	IsInsert bool
}

// ─── FFmpeg continuous HLS encoder ───────────────────────────────────

func (ae *AudioEngine) startFFmpeg() error {
	// Root args for reading from stdin pipe
	args := []string{
		"-f", "s16le",
		"-ar", "44100",
		"-ac", "2",
		"-i", "-",
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11,aresample=44100",
	}

	// For each variant, we add HLS muxer output
	for _, v := range ae.allVariants() {
		os.MkdirAll(v.Dir, 0755)

		hlsFlags := "delete_segments+omit_endlist"
		if v.IsOpus {
			// fMP4 specific flags
			args = append(args,
				"-map", "0:a",
				"-c:a", v.Codec,
				"-b:a", v.Bitrate,
				"-ac", v.Channels, "-ar", v.SampleRate,
				"-f", "hls",
				"-hls_time", strconv.Itoa(ae.station.Config.HlsTime),
				"-hls_list_size", strconv.Itoa(NumSlots),
				"-hls_flags", hlsFlags,
				"-hls_segment_type", "fmp4",
				"-hls_segment_filename", filepath.Join(v.Dir, "seg_%d.mp4"),
				filepath.Join(v.Dir, "index.m3u8"),
			)
		} else {
			// Pure Round Robin using segment muxer
			args = append(args,
				"-map", "0:a",
				"-c:a", v.Codec,
				"-b:a", v.Bitrate,
				"-ac", v.Channels, "-ar", v.SampleRate,
				"-f", "segment",
				"-segment_time", strconv.Itoa(ae.station.Config.HlsTime),
				// Removed -segment_wrap to avoid promotion logic issues
				// Removed -segment_list_flags +live which might cause 'Invalid argument'
				filepath.Join(v.Dir, "raw_seg_%d.ts"),
			)

			// Start a manual playlist manager for this variant
			go ae.manageManualPlaylist(v.Dir)
		}
	}

	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	// Capture stderr directly to the main log for unified debugging
	cmd.Stderr = log.Writer()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	ae.ffmpegCmd = cmd
	ae.ffmpegIn = stdin
	ae.started = true

	activeWriters := []io.Writer{stdin}

	if ae.station.Config.RTMP != "" {
		hasVideo := ae.station.Config.VideoLoop != ""
		hasLogo := ae.station.Config.Logo != ""
		hasText := ae.station.Config.DisplayText != ""

		var rtmpArgs []string
		if hasVideo {
			rtmpArgs = append(rtmpArgs, "-stream_loop", "-1", "-i", ae.station.Config.VideoLoop)
		} else if ae.station.Config.BackgroundImage != "" {
			rtmpArgs = append(rtmpArgs, "-loop", "1", "-i", ae.station.Config.BackgroundImage)
		} else {
			// Misty background fallback
			rtmpArgs = append(rtmpArgs, "-f", "lavfi", "-i", "color=c=#200040:s=640x360:r=30")
		}

		if hasLogo {
			rtmpArgs = append(rtmpArgs, "-i", ae.station.Config.Logo)
		}

		rtmpArgs = append(rtmpArgs, "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "-")

		filter := ""
		if hasLogo && hasText {
			filter = fmt.Sprintf("[0:v]scale=1280:720[bg];[bg][1:v]overlay=W-w-20:20[v1];[v1]drawtext=text='%s':fontcolor=white:fontsize=24:x=(w-text_w)/2:y=(h-text_h)/2", ae.station.Config.DisplayText)
		} else if hasLogo {
			filter = "[0:v]scale=1280:720[bg];[bg][1:v]overlay=W-w-20:20"
		} else if hasText {
			filter = fmt.Sprintf("[0:v]scale=1280:720[bg];[bg]drawtext=text='%s':fontcolor=white:fontsize=24:x=(w-text_w)/2:y=(h-text_h)/2", ae.station.Config.DisplayText)
		} else {
			filter = "[0:v]scale=1280:720"
		}

		rtmpArgs = append(rtmpArgs, "-filter_complex", filter)

		rtmpArgs = append(rtmpArgs,
			"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "800k", "-maxrate", "800k", "-bufsize", "1600k", "-pix_fmt", "yuv420p", "-g", "60",
			"-c:a", "aac", "-b:a", "128k", "-ar", "44100",
			"-f", "flv", ae.station.Config.RTMP,
		)

		rtmpCmd := exec.Command("ffmpeg", rtmpArgs...)
		rtmpStdin, _ := rtmpCmd.StdinPipe()
		rtmpCmd.Stderr = log.Writer()
		if err := rtmpCmd.Start(); err == nil {
			ae.rtmpCmd = rtmpCmd
			log.Printf("%s [Encoder] RTMP Relay started to: %s (VideoLoop=%v, Logo=%v)", ae.station.LogPrefix, ae.station.Config.RTMP, hasVideo, hasLogo)
			activeWriters = append(activeWriters, rtmpStdin)
		} else {
			log.Printf("%s [Encoder] Warning: Failed to start RTMP Relay: %v", ae.station.LogPrefix, err)
		}
	}
	if ae.station.Config.WebRTC {
		var err error
		ae.WebRTCBroadcaster, err = NewWebRTCBroadcaster(ae.station.ID, ae.station.Config.WebRTCNATIP)
		if err == nil {
			log.Printf("%s [Encoder] WebRTC Broadcaster active on local UDP port %d", ae.station.LogPrefix, ae.WebRTCBroadcaster.UDPPort())

			webrtcArgs := []string{
				"-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "-",
				"-af", "loudnorm=I=-16:TP=-1.5:LRA=11,aresample=48000",
				"-c:a", "libopus", "-b:a", "96k", "-vbr", "on", "-compression_level", "10",
				"-payload_type", "111",
				"-f", "rtp", fmt.Sprintf("rtp://127.0.0.1:%d", ae.WebRTCBroadcaster.UDPPort()),
			}
			wCmd := exec.Command("ffmpeg", webrtcArgs...)
			wStdin, _ := wCmd.StdinPipe()
			wCmd.Stderr = log.Writer()
			if err := wCmd.Start(); err == nil {
				ae.webrtcCmd = wCmd
				activeWriters = append(activeWriters, wStdin)
				log.Printf("%s [Encoder] WebRTC RTP Opus encoder spawned successfully", ae.station.LogPrefix)
			} else {
				log.Printf("%s [Encoder] Warning: Failed to start WebRTC RTP encoder: %v", ae.station.LogPrefix, err)
			}
		} else {
			log.Printf("%s [Encoder] Warning: Failed to initialize WebRTC Broadcaster: %v", ae.station.LogPrefix, err)
		}
	}

	if ae.station.Config.MP3 {
		streamArgs := []string{
			"-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "-",
			"-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
			"-c:a", "libmp3lame", "-b:a", "64k", "-f", "mp3", "-flush_packets", "1", "-",
		}
		sCmd := exec.Command("ffmpeg", streamArgs...)
		sStdin, _ := sCmd.StdinPipe()
		sStdout, _ := sCmd.StdoutPipe()
		sCmd.Stderr = log.Writer()
		if err := sCmd.Start(); err == nil {
			ae.streamCmd = sCmd
			go ae.Broadcaster.BroadcastFrom(sStdout)
			
			activeWriters = append(activeWriters, sStdin)
			log.Printf("%s [Encoder] MP3 streamer started successfully", ae.station.LogPrefix)
		} else {
			log.Printf("%s [Encoder] Warning: Failed to start Radio Stream (MP3): %v", ae.station.LogPrefix, err)
		}
	} else {
		log.Printf("%s [Encoder] Radio Stream (MP3) is disabled in config", ae.station.LogPrefix)
	}

	ae.Mixer = NewAudioMixer(NewSafeMultiWriter(activeWriters...), 8)
	go ae.Mixer.Start()

	log.Printf("%s [Encoder] FFmpeg Segmenter & Radio Streamer started", ae.station.LogPrefix)
	log.Printf("%s [Encoder] HLS Command: ffmpeg %s", ae.station.LogPrefix, strings.Join(args, " "))

	// Goroutine to monitor FFmpeg exit
	go func() {
		err := cmd.Wait()
		log.Printf("%s [Encoder] FFmpeg Segmenter exited: %v", ae.station.LogPrefix, err)
		
		ae.mu.Lock()
		ae.started = false
		if ae.Mixer != nil {
			ae.Mixer.Stop()
		}
		if ae.streamCmd != nil && ae.streamCmd.Process != nil {
			ae.streamCmd.Process.Kill()
		}
		if ae.rtmpCmd != nil && ae.rtmpCmd.Process != nil {
			ae.rtmpCmd.Process.Kill()
		}
		ae.mu.Unlock()
	}()

	return nil
}

type variantInfo struct {
	Dir        string
	Codec      string
	Bitrate    string
	Channels   string
	SampleRate string
	Format     string
	Ext        string
	IsOpus     bool
}

func (ae *AudioEngine) allVariants() []variantInfo {
	var variants []variantInfo
	cfg := ae.station.Config

	if cfg.AAC64 {
		variants = append(variants, variantInfo{ae.variants.AAC64, "aac", "64k", "1", "44100", "hls", "ts", false})
	}
	if cfg.AAC96 {
		variants = append(variants, variantInfo{ae.variants.AAC96, "aac", "96k", "2", "44100", "hls", "ts", false})
	}
	if cfg.AAC128 {
		variants = append(variants, variantInfo{ae.variants.AAC128, "aac", "128k", "2", "44100", "hls", "ts", false})
	}
	if cfg.Opus32 {
		variants = append(variants, variantInfo{ae.variants.Opus32, "libopus", "32k", "1", "48000", "hls", "mp4", true})
	}
	if cfg.Opus64 {
		variants = append(variants, variantInfo{ae.variants.Opus64, "libopus", "64k", "2", "48000", "hls", "mp4", true})
	}
	if cfg.Opus96 {
		variants = append(variants, variantInfo{ae.variants.Opus96, "libopus", "96k", "2", "48000", "hls", "mp4", true})
	}
	if cfg.Opus128 {
		variants = append(variants, variantInfo{ae.variants.Opus128, "libopus", "128k", "2", "48000", "hls", "mp4", true})
	}

	// Fallback if none enabled (should not happen with default config)
	if len(variants) == 0 {
		variants = append(variants, variantInfo{ae.variants.AAC128, "aac", "128k", "2", "44100", "hls", "ts", false})
	}

	return variants
}

// ─── Audio Feed ───────────────────────────────────────────────────────

func (ae *AudioEngine) feedStream(args []string, channelID int) error {
	ae.mu.Lock()
	started := ae.started
	mixer := ae.Mixer
	ae.mu.Unlock()

	if !started || mixer == nil {
		return fmt.Errorf("AudioMixer not running")
	}

	// Ambil handle kanal mixer
	targetChannel := mixer.Channels[channelID]

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = targetChannel // Feed ke kanal mixer, bukan langsung ke stdin
	log.Printf("%s [Encoder] Feeding stream to Mixer Channel %d...", ae.station.LogPrefix, channelID)
	
	cmd.Stderr = log.Writer()
	
	ae.mu.Lock()
	ae.channelCmds[channelID] = cmd
	ae.mu.Unlock()

	err := cmd.Run()

	ae.mu.Lock()
	if ae.channelCmds[channelID] == cmd {
		delete(ae.channelCmds, channelID)
	}
	ae.mu.Unlock()

	return err
}

func (ae *AudioEngine) StopChannel(channelID int) {
	ae.mu.Lock()
	cmd := ae.channelCmds[channelID]
	ae.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		log.Printf("[Encoder] Stopping Channel %d process...", channelID)
		cmd.Process.Signal(os.Interrupt)
		// Beri waktu sebentar untuk exit natural, kalau tidak kill
		go func() {
			time.Sleep(500 * time.Millisecond)
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}()
	}
}

// ─── Execute ──────────────────────────────────────────────────────────

func (ae *AudioEngine) Execute(trans Transition) error {
	ae.mu.Lock()
	if !ae.started {
		if err := ae.startFFmpeg(); err != nil {
			ae.mu.Unlock()
			return fmt.Errorf("start ffmpeg: %w", err)
		}
	}
	ae.mu.Unlock()

	var channelID int
	if trans.IsInsert {
		// Kanal 0 untuk prioritas (Insert)
		channelID = 0
	} else {
		// Bergantian Kanal 1 dan 2 untuk Playlist (agar bisa crossfade)
		ae.mu.Lock()
		channelID = ae.nextCh
		if ae.nextCh == 1 {
			ae.nextCh = 2
		} else {
			ae.nextCh = 1
		}
		ae.mu.Unlock()
	}

	log.Printf("%s [Encoder] Playing: %s (Channel: %d, insert=%v)", 
		ae.station.LogPrefix, filepath.Base(trans.NextFile), channelID, trans.IsInsert)
	
	if ae.Mixer != nil {
		ae.Mixer.Channels[channelID].SetLabel(filepath.Base(trans.NextFile))
	}
	
	// Stop previous process on this channel to avoid interleaving data
	ae.StopChannel(channelID)
	
	args := ae.buildFeederArgs(trans.NextFile)

	// Jalankan feeder (blocking dalam goroutine pemanggil Execute)
	if err := ae.feedStream(args, channelID); err != nil {
		if ae.Mixer != nil {
			ae.Mixer.Channels[channelID].SetLabel("")
		}
		return err
	}

	ae.prevFile = trans.NextFile
	log.Printf("%s [Encoder] Channel %d playback finished", ae.station.LogPrefix, channelID)
	if ae.Mixer != nil {
		ae.Mixer.Channels[channelID].SetLabel("")
	}
	return nil
}

// PlayInstant memutar file langsung ke kanal tertentu tanpa menunggu antrian, tapi antre per channel
func (ae *AudioEngine) PlayInstant(file string, channelID int) {
	go func() {
		// Dapatkan atau buat mutex khusus untuk kanal ini
		v, _ := ae.instantMu.LoadOrStore(channelID, &sync.Mutex{})
		mu := v.(*sync.Mutex)
		
		// Antre di sini! Kalau ada orang lain yang lagi Live, tunggu sampai dia selesai
		mu.Lock()
		defer mu.Unlock()

		ae.mu.Lock()
		if !ae.started {
			if err := ae.startFFmpeg(); err != nil {
				ae.mu.Unlock()
				log.Printf("%s [Encoder] PlayInstant failed to start ffmpeg: %v", ae.station.LogPrefix, err)
				return
			}
		}
		ae.mu.Unlock()

		log.Printf("%s [Encoder] PlayInstant: %s (Channel: %d)", 
			ae.station.LogPrefix, filepath.Base(file), channelID)
		
		if ae.Mixer != nil {
			ae.Mixer.Channels[channelID].SetLabel(filepath.Base(file))
		}
		
		ae.StopChannel(channelID)

		args := ae.buildFeederArgs(file)
		if err := ae.feedStream(args, channelID); err != nil {
			log.Printf("%s [Encoder] PlayInstant error: %v", ae.station.LogPrefix, err)
		}

		if ae.Mixer != nil {
			ae.Mixer.Channels[channelID].SetLabel("")
		}
	}()
}

func (ae *AudioEngine) buildFeederArgs(input string) []string {
	return ae.buildFeederArgsWithSeek(input, 0.0)
}

func (ae *AudioEngine) buildFeederArgsWithSeek(input string, seekSeconds float64) []string {
	var inputArgs []string
	isDevice := false

	if strings.HasPrefix(input, "device:") {
		isDevice = true
		parts := strings.SplitN(input, ":", 3)
		if len(parts) >= 2 {
			driver := parts[1] // wasapi, alsa, pulse, dshow
			device := "default"
			if len(parts) == 3 {
				device = parts[2]
			}
			inputArgs = []string{"-f", driver, "-i", device}
		} else {
			inputArgs = []string{"-i", input}
		}
	} else {
		if seekSeconds > 0 {
			inputArgs = []string{"-ss", fmt.Sprintf("%.3f", seekSeconds), "-i", input}
		} else {
			inputArgs = []string{"-i", input}
		}
	}

	args := []string{}
	if !isDevice {
		args = append(args, "-re") // Only use -re for files/URLs
	}
	
	args = append(args, inputArgs...)
	args = append(args,
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
		"-ac", "2", "-ar", "44100",
		"-f", "s16le", "-acodec", "pcm_s16le", "-",
	)
	return args
}

// PlayChannel plays a loaded standby file on a channel strips or resumes playback
func (ae *AudioEngine) PlayChannel(channelID int, volume float64, duration float64) error {
	ae.mu.Lock()
	mixer := ae.Mixer
	ae.mu.Unlock()

	if mixer == nil {
		return fmt.Errorf("mixer not running")
	}
	if channelID < 0 || channelID >= len(mixer.Channels) {
		return fmt.Errorf("invalid channel ID: %d", channelID)
	}

	ch := mixer.Channels[channelID]
	ch.mu.Lock()
	
	file := ch.StandbyFile
	seek := 0.0

	if ch.IsPaused && ch.CurrentPlayingFile != "" {
		file = ch.CurrentPlayingFile
		seek = ch.PausedPosition
		ch.IsPaused = false
	} else {
		if file == "" {
			ch.mu.Unlock()
			return fmt.Errorf("no standby track loaded on channel %d", channelID)
		}
		ch.CurrentPlayingFile = file
		ch.PausedPosition = 0.0
		ch.AccumulatedSeek = 0.0
		ch.IsPaused = false
		ch.TrackDuration = GetAudioDuration(file)
	}
	
	ch.PlayStartTime = time.Now()
	ch.AccumulatedSeek = seek
	ch.mu.Unlock()

	// 1. Set volume to 0.0 instantly
	ch.SetVolume(0.0, 0.0)

	// 2. Play the file in a background goroutine
	go func() {
		v, _ := ae.instantMu.LoadOrStore(channelID, &sync.Mutex{})
		mu := v.(*sync.Mutex)
		
		mu.Lock()
		defer mu.Unlock()

		ae.mu.Lock()
		if !ae.started {
			if err := ae.startFFmpeg(); err != nil {
				ae.mu.Unlock()
				log.Printf("%s [Encoder] PlayChannel failed to start ffmpeg: %v", ae.station.LogPrefix, err)
				return
			}
		}
		ae.mu.Unlock()

		log.Printf("%s [Encoder] PlayChannel: %s (Channel: %d, ss=%.2f)", 
			ae.station.LogPrefix, filepath.Base(file), channelID, seek)
		
		ch.mu.Lock()
		ch.Label = filepath.Base(file)
		ch.mu.Unlock()
		
		ae.StopChannel(channelID)

		args := ae.buildFeederArgsWithSeek(file, seek)
		if err := ae.feedStream(args, channelID); err != nil {
			log.Printf("%s [Encoder] PlayChannel error: %v", ae.station.LogPrefix, err)
		}

		ch.mu.Lock()
		if !ch.IsPaused {
			ch.Label = ""
			ch.CurrentPlayingFile = ""
			ch.StandbyFile = ""
			ch.PausedPosition = 0.0
			ch.AccumulatedSeek = 0.0
		}
		ch.mu.Unlock()
	}()

	// 3. Fade in volume to target over duration
	if duration > 0 {
		ch.SetVolume(volume, duration)
	} else {
		ch.SetVolume(volume, 0.0)
	}

	return nil
}

// PauseChannel pauses a playing channel
func (ae *AudioEngine) PauseChannel(channelID int) error {
	ae.mu.Lock()
	mixer := ae.Mixer
	ae.mu.Unlock()

	if mixer == nil {
		return fmt.Errorf("mixer not running")
	}
	if channelID < 0 || channelID >= len(mixer.Channels) {
		return fmt.Errorf("invalid channel ID: %d", channelID)
	}

	ch := mixer.Channels[channelID]
	ch.mu.Lock()
	if !ch.active || ch.IsPaused {
		ch.mu.Unlock()
		return nil
	}

	elapsed := time.Since(ch.PlayStartTime).Seconds() + ch.AccumulatedSeek
	ch.PausedPosition = elapsed
	ch.IsPaused = true
	ch.mu.Unlock()

	ae.StopChannel(channelID)

	log.Printf("[Encoder] Paused Channel %d at %.2f seconds", channelID, elapsed)
	return nil
}

// RewindChannel rewinds a channel by some seconds
func (ae *AudioEngine) RewindChannel(channelID int, seconds float64) error {
	ae.mu.Lock()
	mixer := ae.Mixer
	ae.mu.Unlock()

	if mixer == nil {
		return fmt.Errorf("mixer not running")
	}
	if channelID < 0 || channelID >= len(mixer.Channels) {
		return fmt.Errorf("invalid channel ID: %d", channelID)
	}

	ch := mixer.Channels[channelID]
	ch.mu.Lock()
	
	if ch.CurrentPlayingFile == "" {
		ch.mu.Unlock()
		return fmt.Errorf("no track playing on channel %d", channelID)
	}

	var elapsed float64
	if ch.IsPaused {
		elapsed = ch.PausedPosition
	} else {
		elapsed = time.Since(ch.PlayStartTime).Seconds() + ch.AccumulatedSeek
	}

	newPosition := elapsed - seconds
	if newPosition < 0 {
		newPosition = 0.0
	}

	ch.AccumulatedSeek = newPosition
	ch.PlayStartTime = time.Now()
	
	if ch.IsPaused {
		ch.PausedPosition = newPosition
		ch.mu.Unlock()
		log.Printf("[Encoder] Rewinded paused Channel %d to %.2f seconds", channelID, newPosition)
		return nil
	}
	
	file := ch.CurrentPlayingFile
	ch.mu.Unlock()

	log.Printf("[Encoder] Rewinding active Channel %d to %.2f seconds", channelID, newPosition)

	go func() {
		v, _ := ae.instantMu.LoadOrStore(channelID, &sync.Mutex{})
		mu := v.(*sync.Mutex)
		
		mu.Lock()
		defer mu.Unlock()

		ae.StopChannel(channelID)

		args := ae.buildFeederArgsWithSeek(file, newPosition)
		if err := ae.feedStream(args, channelID); err != nil {
			log.Printf("%s [Encoder] Rewind playback error: %v", ae.station.LogPrefix, err)
		}

		ch.mu.Lock()
		if !ch.IsPaused {
			ch.Label = ""
			ch.CurrentPlayingFile = ""
			ch.StandbyFile = ""
			ch.PausedPosition = 0.0
			ch.AccumulatedSeek = 0.0
		}
		ch.mu.Unlock()
	}()

	return nil
}

func (ae *AudioEngine) manageManualPlaylist(dir string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	durations := make(map[int]float64)
	lastMods := make(map[int]time.Time)

	// Bersihkan sampah raw_seg_* lama agar tidak mengacaukan perhitungan maxRawIdx.
	// KITA MEMBIARKAN seg_*.ts (file jadi) agar player pendengar tidak terputus saat restart.
	if files, err := filepath.Glob(filepath.Join(dir, "raw_seg_*.ts")); err == nil {
		for _, f := range files {
			os.Remove(f)
		}
	}

	// Pre-scan file yang sudah ada di disk agar langsung sinkron
	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, fmt.Sprintf("seg_%d.ts", i))
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			durations[i] = GetAudioDuration(path)
			lastMods[i] = info.ModTime()
		}
	}

	for {
		ae.mu.Lock()
		started := ae.started
		ae.mu.Unlock()
		if !started {
			return
		}

		// Tahap 1: Cari semua file raw yang ada
		var rawFiles []string
		if matches, err := filepath.Glob(filepath.Join(dir, "raw_seg_*.ts")); err == nil {
			rawFiles = matches
		}

		var maxRawIdx int = -1
		for _, path := range rawFiles {
			base := filepath.Base(path)
			var idx int
			if n, err := fmt.Sscanf(base, "raw_seg_%d.ts", &idx); err == nil && n == 1 {
				if idx > maxRawIdx {
					maxRawIdx = idx
				}
			}
		}

		// Tahap 2: Pindahkan file raw yang sudah selesai (index < maxRawIdx)
		for _, path := range rawFiles {
			base := filepath.Base(path)
			var idx int
			if n, err := fmt.Sscanf(base, "raw_seg_%d.ts", &idx); err == nil && n == 1 {
				// File dengan indeks lebih kecil dari yang terbaru PASTI sudah selesai
				if idx < maxRawIdx {
					targetIdx := idx % 10
					targetPath := filepath.Join(dir, fmt.Sprintf("seg_%d.ts", targetIdx))
					
					if info, err := os.Stat(path); err == nil && info.Size() > 0 {
						// Double check: don't promote if it was modified VERY recently (within 500ms)
						// to avoid race condition with FFmpeg closing the file
						if time.Since(info.ModTime()) > 500*time.Millisecond {
							err := os.Rename(path, targetPath)
							if err != nil {
								log.Printf("[HLS] [%s] Gagal Rename %s -> %s: %v", filepath.Base(dir), base, fmt.Sprintf("seg_%d.ts", targetIdx), err)
							} else {
								log.Printf("[HLS] [%s] Promoted %s -> %s (Mantap)", filepath.Base(dir), base, fmt.Sprintf("seg_%d.ts", targetIdx))
							}
						}
					}
				}
			} else {
				// Jika namanya aneh, hapus saja
				os.Remove(path)
			}
		}
		// Tahap 3: Scan file target yang sudah "mantap"
		var newestTargetIdx int = -1
		var maxTargetMod time.Time
		for i := 0; i < 10; i++ {
			path := filepath.Join(dir, fmt.Sprintf("seg_%d.ts", i))
			info, err := os.Stat(path)
			if err == nil && info.Size() > 0 {
				if info.ModTime().After(maxTargetMod) {
					maxTargetMod = info.ModTime()
					newestTargetIdx = i
				}
				// Selalu update cache jika ModTime berubah
				if info.ModTime().After(lastMods[i]) {
					durations[i] = GetAudioDuration(path)
					lastMods[i] = info.ModTime()
				}
			}
		}

		// Update: Sekarang kita bisa update playlist meski belum 10 file (minimal 3 agar player stabil)
		numSegs := len(durations)
		if newestTargetIdx == -1 || numSegs < 3 {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Cari urutan pertama (berdasarkan index asli FFmpeg agar naik tepat 1 angka tiap segmen)
		// Jika maxRawIdx=78 dan ada 10 segmen, maka urutan pertamanya adalah 69.
		sequence := maxRawIdx - numSegs + 1
		if sequence < 0 {
			sequence = 0
		}
		firstIdx := (newestTargetIdx - numSegs + 1 + 10) % 10

		var sb strings.Builder
		sb.WriteString("#EXTM3U\n")
		sb.WriteString("#EXT-X-VERSION:3\n")
		sb.WriteString("#EXT-X-ALLOW-CACHE:NO\n")
		sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", ae.station.Config.HlsTime))
		sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", sequence))

		for i := 0; i < numSegs; i++ {
			idx := (firstIdx + i) % 10
			dur := durations[idx]
			if dur <= 0 {
				dur = float64(ae.station.Config.HlsTime)
			}
			sb.WriteString(fmt.Sprintf("#EXTINF:%.6f,\n", dur))
			sb.WriteString(fmt.Sprintf("seg_%d.ts?t=%d\n", idx, lastMods[idx].Unix()))
		}

		os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(sb.String()), 0644)
		time.Sleep(1 * time.Second)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────

func GetAudioDuration(path string) float64 {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	).Output()
	if err != nil || len(out) == 0 {
		return 0
	}
	dur, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return dur
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (ae *AudioEngine) Reset() {
	ae.mu.Lock()
	defer ae.mu.Unlock()

	if ae.ffmpegCmd != nil && ae.ffmpegCmd.Process != nil {
		ae.ffmpegCmd.Process.Kill()
	}
	if ae.streamCmd != nil && ae.streamCmd.Process != nil {
		ae.streamCmd.Process.Kill()
	}
	if ae.rtmpCmd != nil && ae.rtmpCmd.Process != nil {
		ae.rtmpCmd.Process.Kill()
	}
	if ae.webrtcCmd != nil && ae.webrtcCmd.Process != nil {
		ae.webrtcCmd.Process.Kill()
	}
	ae.ffmpegCmd = nil
	ae.streamCmd = nil
	ae.rtmpCmd = nil
	ae.webrtcCmd = nil
	if ae.WebRTCBroadcaster != nil {
		ae.WebRTCBroadcaster.Close()
		ae.WebRTCBroadcaster = nil
	}
	ae.ffmpegIn = nil
	ae.started = false
	ae.prevFile = ""

	os.RemoveAll(ae.tempDir)
	os.MkdirAll(ae.tempDir, 0755)
}

// SafeMultiWriter is an io.Writer that writes to all sub-writers independently.
// If any writer returns an error (like EPIPE for broken pipes), it ignores it
// and proceeds with the rest of the writers in the list, ensuring that a failing
// RTMP stream or other output does not stall or break the rest of the stream chain.
type SafeMultiWriter struct {
	writers []io.Writer
}

func NewSafeMultiWriter(writers ...io.Writer) *SafeMultiWriter {
	var clean []io.Writer
	for _, w := range writers {
		if w != nil {
			clean = append(clean, w)
		}
	}
	return &SafeMultiWriter{writers: clean}
}

func (s *SafeMultiWriter) Write(p []byte) (n int, err error) {
	for _, w := range s.writers {
		// Ignore write errors to broken or closed processes
		w.Write(p)
	}
	return len(p), nil
}
