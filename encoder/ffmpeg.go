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

	"github.com/streamer/types"
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

	var multiOut io.Writer = stdin

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
			multiOut = io.MultiWriter(multiOut, rtmpStdin)
		} else {
			log.Printf("%s [Encoder] Warning: Failed to start RTMP Relay: %v", ae.station.LogPrefix, err)
		}
	}
	if ae.station.Config.MP3 {
		streamArgs := []string{
			"-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "-",
			"-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
			"-c:a", "libmp3lame", "-b:a", "128k", "-f", "mp3", "-",
		}
		sCmd := exec.Command("ffmpeg", streamArgs...)
		sStdin, _ := sCmd.StdinPipe()
		sStdout, _ := sCmd.StdoutPipe()
		sCmd.Stderr = log.Writer()
		if err := sCmd.Start(); err == nil {
			ae.streamCmd = sCmd
			go ae.Broadcaster.BroadcastFrom(sStdout)
			
			// Combine all active outputs
			multiOut = io.MultiWriter(multiOut, sStdin)
			ae.Mixer = NewAudioMixer(multiOut, 8)
		} else {
			log.Printf("%s [Encoder] Warning: Failed to start Radio Stream (MP3): %v", ae.station.LogPrefix, err)
			ae.Mixer = NewAudioMixer(multiOut, 8)
		}
	} else {
		log.Printf("%s [Encoder] Radio Stream (MP3) is disabled in config", ae.station.LogPrefix)
		ae.Mixer = NewAudioMixer(multiOut, 8)
	}
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

	// Get the mixer channel handle
	targetChannel := mixer.Channels[channelID]

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = targetChannel // Feed to mixer channel, not directly to stdin
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
		// Give it a moment to exit naturally, otherwise force kill
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
		// Channel 0 is reserved for high-priority (Insert) audio
		channelID = 0
	} else {
		// Alternate between Channel 1 and 2 for Playlist (to enable crossfading)
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

	// Run feeder (blocking in the goroutine calling Execute)
	if err := ae.feedStream(args, channelID); err != nil {
		return err
	}

	ae.prevFile = trans.NextFile
	log.Printf("%s [Encoder] Channel %d playback finished", ae.station.LogPrefix, channelID)
	return nil
}

// PlayInstant plays a file directly to a specific channel without waiting in the queue
func (ae *AudioEngine) PlayInstant(file string, channelID int) {
	go func() {
		log.Printf("%s [Encoder] PlayInstant: %s (Channel: %d)", 
			ae.station.LogPrefix, filepath.Base(file), channelID)
		
		if ae.Mixer != nil {
			ae.Mixer.Channels[channelID].SetLabel(filepath.Base(file))
		}
		
		args := ae.buildFeederArgs(file)
		if err := ae.feedStream(args, channelID); err != nil {
			log.Printf("%s [Encoder] PlayInstant error: %v", ae.station.LogPrefix, err)
		}
	}()
}

func (ae *AudioEngine) buildFeederArgs(input string) []string {
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
		inputArgs = []string{"-i", input}
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

func (ae *AudioEngine) manageManualPlaylist(dir string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	durations := make(map[int]float64)
	lastMods := make(map[int]time.Time)

	// Clean up old raw_seg_* files so they don't mess up maxRawIdx calculation.
	// WE KEEP seg_*.ts (final files) so listeners are not disconnected upon restart.
	if files, err := filepath.Glob(filepath.Join(dir, "raw_seg_*.ts")); err == nil {
		for _, f := range files {
			os.Remove(f)
		}
	}

	// Pre-scan existing files on disk to immediately sync
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

		// Stage 1: Find all existing raw files
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

		// Stage 2: Move completed raw files (index < maxRawIdx)
		for _, path := range rawFiles {
			base := filepath.Base(path)
			var idx int
			if n, err := fmt.Sscanf(base, "raw_seg_%d.ts", &idx); err == nil && n == 1 {
				// Files with an index smaller than the newest one are DEFINITELY complete
				if idx < maxRawIdx {
					targetIdx := idx % 10
					targetPath := filepath.Join(dir, fmt.Sprintf("seg_%d.ts", targetIdx))
					
					if info, err := os.Stat(path); err == nil && info.Size() > 0 {
						// Double check: don't promote if it was modified VERY recently (within 500ms)
						// to avoid race condition with FFmpeg closing the file
						if time.Since(info.ModTime()) > 500*time.Millisecond {
							err := os.Rename(path, targetPath)
							if err != nil {
								log.Printf("[HLS] [%s] Failed to rename %s -> %s: %v", filepath.Base(dir), base, fmt.Sprintf("seg_%d.ts", targetIdx), err)
							} else {
								log.Printf("[HLS] [%s] Promoted %s -> %s (Success)", filepath.Base(dir), base, fmt.Sprintf("seg_%d.ts", targetIdx))
							}
						}
					}
				}
			} else {
				// If the name is weird, just delete it
				os.Remove(path)
			}
		}
		// Stage 3: Scan target files that are already "solid"
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
				// Always update cache if ModTime changes
				if info.ModTime().After(lastMods[i]) {
					durations[i] = GetAudioDuration(path)
					lastMods[i] = info.ModTime()
				}
			}
		}

		// Update: We can now update the playlist even without 10 files (min 3 for player stability)
		numSegs := len(durations)
		if newestTargetIdx == -1 || numSegs < 3 {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Find the starting sequence (based on original FFmpeg index so it increments exactly by 1)
		// If maxRawIdx=78 and there are 10 segments, the starting sequence is 69.
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
	ae.ffmpegCmd = nil
	ae.streamCmd = nil
	ae.rtmpCmd = nil
	ae.ffmpegIn = nil
	ae.started = false
	ae.prevFile = ""

	os.RemoveAll(ae.tempDir)
	os.MkdirAll(ae.tempDir, 0755)
}
