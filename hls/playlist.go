package hls

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hrizal/GO-AUDIO-BROADCASTER/types"
)

const (
	MaxSegments    = 10
	SegmentDur     = 10
	SegmentPrefix  = "seg_"
)

// SegmentManager manages the rolling window of segments
type SegmentManager struct {
	station    *types.Station
	variants   types.BitrateVariants
	mu         sync.Mutex
	segCounter int
}

func NewSegmentManager(station *types.Station, variants types.BitrateVariants) *SegmentManager {
	return &SegmentManager{
		station:    station,
		variants:   variants,
		segCounter: 1,
	}
}

func (sm *SegmentManager) NextSegmentName() string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	name := fmt.Sprintf("%s%04d.ts", SegmentPrefix, sm.segCounter)
	sm.segCounter++
	// Keep incrementing, no rolling — unique filenames for live HLS
	return name
}

func (sm *SegmentManager) NextSegmentNameOpus() string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	name := fmt.Sprintf("%s%04d.mp4", SegmentPrefix, sm.segCounter)
	sm.segCounter++
	return name
}

func (sm *SegmentManager) RollingSegmentIndex() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	idx := sm.segCounter
	sm.segCounter++
	return idx
}

func (sm *SegmentManager) ResetCounter() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	// Don't reset to 1, keep incrementing to avoid name collision
	// Just ensure we start high enough after a reset
}

// CollectExistingSegments finds segment files in a directory (.ts or .mp4)
func CollectExistingSegments(dir string) []string {
	pattern := filepath.Join(dir, "seg_*.*")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	result := make([]string, len(files))
	for i, f := range files {
		result[i] = filepath.Base(f)
	}
	return result
}

// CollectSegmentsByExt finds segment files with specific extension
func CollectSegmentsByExt(dir string, ext string) []string {
	pattern := filepath.Join(dir, "seg_*."+ext)
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	result := make([]string, len(files))
	for i, f := range files {
		result[i] = filepath.Base(f)
	}
	return result
}

// GenerateVariantPlaylist creates a variant .m3u8 playlist
func GenerateVariantPlaylist(variantDir, bitrate string, segments []string, sequenceNum int) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-VERSION:3\n"))
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", SegmentDur))
	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", sequenceNum))

	for _, seg := range segments {
		sb.WriteString(fmt.Sprintf("#EXTINF:%.1f,\n", float64(SegmentDur)))
		sb.WriteString(seg + "\n")
	}

	return sb.String()
}

// GenerateOpusVariantPlaylist creates a variant playlist for Opus fMP4 segments
func GenerateOpusVariantPlaylist(variantDir, bitrate string, segments []string, sequenceNum int) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-VERSION:7\n"))
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", SegmentDur))
	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", sequenceNum))

	for _, seg := range segments {
		sb.WriteString(fmt.Sprintf("#EXTINF:%.1f,\n", float64(SegmentDur)))
		sb.WriteString(seg + "\n")
	}

	return sb.String()
}

func WriteVariantPlaylist(variantDir, content string) error {
	// Atomic write: write to .tmp first, then rename
	// This prevents HLS clients from reading a partially-written playlist
	tmpPath := filepath.Join(variantDir, "index.m3u8.tmp")
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return err
	}
	playlistPath := filepath.Join(variantDir, "index.m3u8")
	return os.Rename(tmpPath, playlistPath)
}

// GenerateMasterPlaylist creates master playlist with AAC + Opus variants
func GenerateMasterPlaylist(stationID string, variants types.BitrateVariants) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:7\n\n")

	// ─── AAC variants (.ts, native HLS) ───
	sb.WriteString("# AAC (native HLS, universal)\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"mp4a.40.2\",CHANNELS=\"1\"\n", 64000))
	sb.WriteString("aac/64k/index.m3u8\n\n")

	sb.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"mp4a.40.2\",CHANNELS=\"2\"\n", 96000))
	sb.WriteString("aac/96k/index.m3u8\n\n")

	sb.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"mp4a.40.2\",CHANNELS=\"2\"\n", 128000))
	sb.WriteString("aac/128k/index.m3u8\n\n")

	// ─── Opus variants (fMP4, modern hls.js) ───
	sb.WriteString("# Opus (hls.js, modern browsers)\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"opus\",CHANNELS=\"1\"\n", 32000))
	sb.WriteString("opus/32k/index.m3u8\n\n")

	sb.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"opus\",CHANNELS=\"2\"\n", 64000))
	sb.WriteString("opus/64k/index.m3u8\n\n")

	sb.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS=\"opus\",CHANNELS=\"2\"\n", 96000))
	sb.WriteString("opus/96k/index.m3u8\n")

	return sb.String()
}

func WriteMasterPlaylist(stationDir, content string) error {
	// Atomic write: write to .tmp first, then rename
	tmpPath := filepath.Join(stationDir, "master.m3u8.tmp")
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return err
	}
	playlistPath := filepath.Join(stationDir, "master.m3u8")
	return os.Rename(tmpPath, playlistPath)
}

// EnsureOutputDirs creates all variant output directories
func EnsureOutputDirs(variants types.BitrateVariants) error {
	for _, dir := range variants.All() {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create dir %s: %w", dir, err)
		}
	}
	return nil
}

// CleanupSegmentsNotInSet removes segment files not in the given set
func CleanupSegmentsNotInSet(dir string, keepSet map[string]bool) {
	pattern := filepath.Join(dir, "seg_*.*")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, f := range files {
		name := filepath.Base(f)
		if !keepSet[name] {
			if err := os.Remove(f); err != nil {
				log.Printf("[Cleanup] Warning: failed to remove %s: %v", f, err)
			}
		}
	}
}

func CleanupAllSegmentsForVariant(variantDir string) {
	pattern := filepath.Join(variantDir, "seg_*.*")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			log.Printf("[Cleanup] Warning: failed to remove %s: %v", f, err)
		}
	}
}

func CleanupAllSegments(variants types.BitrateVariants) error {
	for _, dir := range variants.All() {
		pattern := filepath.Join(dir, "seg_*.*")
		files, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, f := range files {
			if err := os.Remove(f); err != nil {
				log.Printf("[Cleanup] Warning: failed to remove %s: %v", f, err)
			}
		}
	}
	return nil
}

func CleanupOldSegments(dir string) {
	segments := CollectExistingSegments(dir)
	if len(segments) > MaxSegments {
		for _, seg := range segments[:len(segments)-MaxSegments] {
			segPath := filepath.Join(dir, seg)
			if err := os.Remove(segPath); err != nil {
				log.Printf("[Cleanup] Warning: failed to remove %s: %v", segPath, err)
			}
		}
	}
}
