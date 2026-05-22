package station

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrizal/GO-AUDIO-BROADCASTER/encoder"
	"github.com/hrizal/GO-AUDIO-BROADCASTER/hls"
	"github.com/hrizal/GO-AUDIO-BROADCASTER/types"
)

const (
	SilentFile     = "silent_5s.mp3"
	SilentDuration = 5
)

type StationRunner struct {
	Station    *types.Station
	QueueMgr   *QueueManager
	Encoder    *encoder.AudioEngine
	Variants   types.BitrateVariants
	silentPath string
}

func NewStationRunner(station *types.Station, silentDir string) *StationRunner {
	variants := types.NewBitrateVariants(station.OutputDir)
	enc := encoder.NewAudioEngine(station, variants)

	return &StationRunner{
		Station:    station,
		QueueMgr:   NewQueueManager(station),
		Encoder:    enc,
		Variants:   variants,
		silentPath: filepath.Join(silentDir, SilentFile),
	}
}

func (sr *StationRunner) InitStation() error {
	if err := hls.EnsureOutputDirs(sr.Variants); err != nil {
		return fmt.Errorf("failed to create output dirs: %w", err)
	}

	masterContent := hls.GenerateMasterPlaylist(sr.Station.ID, sr.Variants)
	if err := hls.WriteMasterPlaylist(sr.Station.OutputDir, masterContent); err != nil {
		return fmt.Errorf("failed to write initial master playlist: %w", err)
	}

	// AAC variant playlists
	for _, dir := range sr.Variants.AllAAC() {
		content := hls.GenerateVariantPlaylist(dir, "", nil, 1)
		if err := hls.WriteVariantPlaylist(dir, content); err != nil {
			return fmt.Errorf("failed to write initial AAC variant %s: %w", dir, err)
		}
	}

	// Opus variant playlists
	for _, dir := range sr.Variants.AllOpus() {
		content := hls.GenerateOpusVariantPlaylist(dir, "", nil, 1)
		if err := hls.WriteVariantPlaylist(dir, content); err != nil {
			return fmt.Errorf("failed to write initial Opus variant %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(sr.silentPath); os.IsNotExist(err) {
		log.Printf("%s Silent file not found, generating...", sr.Station.LogPrefix)
		if err := sr.generateSilentFile(); err != nil {
			return fmt.Errorf("failed to generate silent file: %w", err)
		}
	}

	// Restore queue from disk (if exists)
	if err := sr.QueueMgr.Load(); err != nil {
		log.Printf("%s Warning: failed to restore queue: %v", sr.Station.LogPrefix, err)
	}

	return nil
}

func (sr *StationRunner) generateSilentFile() error {
	args := []string{
		"-f", "lavfi",
		"-i", "anullsrc=r=44100:cl=mono",
		"-t", "5",
		"-c:a", "libmp3lame",
		"-q:a", "9",
		"-y", sr.silentPath,
	}
	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg silent generation error: %w\noutput: %s", err, output)
	}
	log.Printf("%s Generated silent file: %s", sr.Station.LogPrefix, sr.silentPath)
	return nil
}

func (sr *StationRunner) getStationDir() string {
	return sr.Station.OutputDir
}

func (sr *StationRunner) Run() {
	log.Printf("%s Station goroutine started", sr.Station.LogPrefix)

	for {
		select {
		case <-sr.Station.StopChan():
			log.Printf("%s Station goroutine stopped", sr.Station.LogPrefix)
			return
		default:
			sr.runLoop()
			log.Printf("%s Station restarting from silent (self-heal)...", sr.Station.LogPrefix)
			sr.selfHeal()
		}
	}
}

func (sr *StationRunner) runLoop() {
	currentFile := sr.silentPath

	// Bootstrap: encode silent filler
	if err := sr.Encoder.Execute(encoder.Transition{
		PrevFile: "",
		NextFile: currentFile,
		IsInsert: true,
	}); err != nil {
		log.Printf("%s Bootstrap silent failed: %v (will retry)", sr.Station.LogPrefix, err)
		time.Sleep(2 * time.Second)
		return
	}

	log.Printf("%s Bootstrap silent completed. Entering queue processing loop.", sr.Station.LogPrefix)

	for {
		select {
		case <-sr.Station.StopChan():
			log.Printf("%s Station stop signal received", sr.Station.LogPrefix)
			return
		default:
			// If in manual mode, wait and do nothing in the auto loop
			sr.Station.RLock()
			isManual := sr.Station.Mode == "manual"
			sr.Station.RUnlock()

			if isManual {
				time.Sleep(500 * time.Millisecond)
				continue
			}

			nextFile, nextIsInsert := sr.popTrack()
			if nextFile == "" {
				// No tracks, play silent
				if currentFile == sr.silentPath {
					time.Sleep(SilentDuration * time.Second)
					continue
				}
				nextFile = sr.silentPath
				nextIsInsert = true
				sr.Station.Lock()
				sr.Station.Status = types.StatusSilent
				sr.Station.CurrentTrack = "silent_5s.mp3"
				sr.Station.NextTrack = ""
				sr.Station.Unlock()
			}

			// Determine track duration
			dur := encoder.GetAudioDuration(nextFile)
			if dur <= 0 {
				dur = 5.0 // Fallback
			}

			// Update Station Status
			sr.Station.Lock()
			sr.Station.PreviousTrack = filepath.Base(currentFile)
			sr.Station.CurrentTrack = filepath.Base(nextFile)
			sr.Station.CurrentFile = nextFile
			if !nextIsInsert {
				sr.Station.Status = types.StatusPlaying
			} else {
				sr.Station.Status = types.StatusInsert
			}
			sr.Station.Unlock()

			// START TRACK (Async to allow overlapping/mixing)
			go func(file string, insert bool) {
				sr.Encoder.Execute(encoder.Transition{
					NextFile: file,
					IsInsert: insert,
				})
			}(nextFile, nextIsInsert)

			// WAIT LOGIC (For Crossfade)
			// Rule: Playlist to Playlist = Wait Duration - 3 seconds
			// Otherwise = Wait Full Duration
			peekFile, peekIsInsert := sr.QueueMgr.PeekNextFile()
			
			waitSec := dur
			if !nextIsInsert && !peekIsInsert && peekFile != "" && nextFile != sr.silentPath {
				// OK for Crossfade
				waitSec = dur - float64(sr.Station.Config.Crossfade)
				if waitSec < 1 { waitSec = 1 } // Minimum 1 second before overlap
				log.Printf("%s Mixer: Crossfade planned in %.2fs (config: %ds)", sr.Station.LogPrefix, waitSec, sr.Station.Config.Crossfade)
			} else {
				log.Printf("%s Mixer: Normal playback, waiting full %.2fs", sr.Station.LogPrefix, waitSec)
			}

			currentFile = nextFile
			
			// WAIT LOGIC (Using Select for interruptibility)
			timer := time.NewTimer(time.Duration(waitSec * float64(time.Second)))
			select {
			case <-sr.Station.StopChan():
				timer.Stop()
				return
			case <-sr.Station.SkipChan():
				timer.Stop()
				log.Printf("%s [Mixer] Skip/Restart signal received. Interrupting current channels.", sr.Station.LogPrefix)
				sr.Encoder.StopChannel(1)
				sr.Encoder.StopChannel(2)
				sr.Encoder.StopChannel(0) // Also insert channel if necessary
			case <-timer.C:
				// Normal end, continue to next track
			}
		}
	}
}

func (sr *StationRunner) selfHeal() {
	log.Printf("%s [SELF-HEAL] Starting self-healing process...", sr.Station.LogPrefix)
	sr.Encoder.Reset()
	sr.Station.Lock()
	sr.Station.InsertQueue = make([]string, 0)
	sr.Station.PlaylistQueue = make([]string, 0)
	sr.Station.CurrentTrack = "silent_5s.mp3"
	sr.Station.Status = types.StatusSilent
	sr.Station.Unlock()
	time.Sleep(2 * time.Second)
	log.Printf("%s [SELF-HEAL] Self-healing complete. Restarting from silent.", sr.Station.LogPrefix)
}

func (sr *StationRunner) popTrack() (string, bool) {
	file, isInsert := sr.QueueMgr.NextFile()
	if file == "" {
		sr.Station.Lock()
		sr.Station.NextTrack = ""
		sr.Station.Unlock()
		return "", false
	}
	if !strings.HasPrefix(file, "http://") && !strings.HasPrefix(file, "https://") {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			log.Printf("%s File not found: %s (skipping)", sr.Station.LogPrefix, file)
			return sr.popTrack()
		}
	}
	nextFile, _ := sr.QueueMgr.PeekNextFile()
	sr.Station.Lock()
	if nextFile != "" {
		sr.Station.NextTrack = filepath.Base(nextFile)
	} else {
		sr.Station.NextTrack = ""
	}
	sr.Station.Unlock()
	return file, isInsert
}

func (sr *StationRunner) GenerateSilentPath() string {
	return sr.silentPath
}

func (sr *StationRunner) GenerateSilentFilePublic() error {
	return sr.generateSilentFile()
}
