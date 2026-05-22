package station

import (
	"encoding/json"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"

	"github.com/hrizal/GO-AUDIO-BROADCASTER/types"
)

// persistedQueue is the on-disk format for queue persistence
type persistedQueue struct {
	PlaylistQueue []string `json:"playlist"`
	OriginalQueue []string `json:"original"`
	PlayedSet     []string `json:"played"`
}

// QueueManager manages the insert_queue and playlist_queue for a station
type QueueManager struct {
	station *types.Station
	mu      sync.Mutex
}

func NewQueueManager(station *types.Station) *QueueManager {
	return &QueueManager{station: station}
}

// queueFilePath returns the path to the queue persistence file
func (qm *QueueManager) queueFilePath() string {
	return filepath.Join(qm.station.OutputDir, ".queue.json")
}

// Save persists current queue state to disk
func (qm *QueueManager) Save() error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	pq := persistedQueue{
		PlaylistQueue: qm.station.PlaylistQueue,
		OriginalQueue: qm.station.OriginalQueue,
	}
	for k := range qm.station.PlayedSet {
		pq.PlayedSet = append(pq.PlayedSet, k)
	}

	data, err := json.Marshal(pq)
	if err != nil {
		return err
	}
	return os.WriteFile(qm.queueFilePath(), data, 0644)
}

// Load restores queue state from disk (if exists)
func (qm *QueueManager) Load() error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	path := qm.queueFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // not an error, just no saved state
		}
		return err
	}

	var pq persistedQueue
	if err := json.Unmarshal(data, &pq); err != nil {
		return err
	}

	qm.station.PlaylistQueue = pq.PlaylistQueue
	qm.station.OriginalQueue = pq.OriginalQueue
	qm.station.PlayedSet = make(map[string]bool)
	for _, k := range pq.PlayedSet {
		qm.station.PlayedSet[k] = true
	}

	log.Printf("%s Queue restored from disk: %d playlist files, %d played set entries",
		qm.station.LogPrefix, len(pq.PlaylistQueue), len(pq.PlayedSet))
	return nil
}

func (qm *QueueManager) PushInsert(files []string) {
	qm.mu.Lock()
	qm.station.InsertQueue = append(qm.station.InsertQueue, files...)
	log.Printf("%s Insert queue +%d files (total: %d)", qm.station.LogPrefix, len(files), len(qm.station.InsertQueue))
	qm.mu.Unlock()
	qm.Save()
}

func (qm *QueueManager) PushPlaylist(files []string) {
	qm.mu.Lock()
	qm.station.PlaylistQueue = append(qm.station.PlaylistQueue, files...)
	qm.station.OriginalQueue = append(qm.station.OriginalQueue, files...)
	log.Printf("%s Playlist queue +%d files (total: %d)", qm.station.LogPrefix, len(files), len(qm.station.PlaylistQueue))
	qm.mu.Unlock()
	qm.Save()
}

func (qm *QueueManager) ReplacePlaylist(files []string) {
	qm.mu.Lock()
	oldLen := len(qm.station.PlaylistQueue)
	qm.station.PlaylistQueue = make([]string, len(files))
	copy(qm.station.PlaylistQueue, files)
	qm.station.OriginalQueue = make([]string, len(files))
	copy(qm.station.OriginalQueue, files)
	qm.station.PlayedSet = make(map[string]bool)
	log.Printf("%s Playlist queue REPLACED: %d removed, %d new files (total: %d)",
		qm.station.LogPrefix, oldLen, len(files), len(qm.station.PlaylistQueue))
	qm.mu.Unlock()
	qm.Save()
}

func (qm *QueueManager) NextFile() (string, bool) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if len(qm.station.InsertQueue) > 0 {
		file := qm.station.InsertQueue[0]
		qm.station.InsertQueue = qm.station.InsertQueue[1:]
		log.Printf("%s Pop from INSERT queue: %s (remaining: %d)",
			qm.station.LogPrefix, file, len(qm.station.InsertQueue))
		return file, true
	}

	if len(qm.station.PlaylistQueue) == 0 && qm.station.Config.Loop && len(qm.station.OriginalQueue) > 0 {
		qm.station.PlaylistQueue = make([]string, len(qm.station.OriginalQueue))
		copy(qm.station.PlaylistQueue, qm.station.OriginalQueue)
		qm.station.PlayedSet = make(map[string]bool)
		log.Printf("%s Playlist queue LOOPED (%d files)", qm.station.LogPrefix, len(qm.station.PlaylistQueue))
	}

	if len(qm.station.PlaylistQueue) > 0 {
		var idx int
		if qm.station.Config.Random {
			idx = rand.Intn(len(qm.station.PlaylistQueue))
		}

		file := qm.station.PlaylistQueue[idx]
		basename := filepath.Base(file)

		if qm.station.Config.Unique && qm.station.Config.Random {
			if qm.station.PlayedSet[basename] || qm.station.PlayedSet[file] {
				qm.station.PlaylistQueue = append(qm.station.PlaylistQueue[:idx], qm.station.PlaylistQueue[idx+1:]...)
				if len(qm.station.PlaylistQueue) == 0 {
					qm.station.PlayedSet = make(map[string]bool)
				}
				return qm.popPlaylist()
			}
		}

		qm.station.PlaylistQueue = append(qm.station.PlaylistQueue[:idx], qm.station.PlaylistQueue[idx+1:]...)
		qm.station.PlayedSet[basename] = true
		qm.station.PlayedSet[file] = true

		log.Printf("%s Pop from PLAYLIST queue: %s (remaining: %d)",
			qm.station.LogPrefix, file, len(qm.station.PlaylistQueue))
		return file, false
	}

	log.Printf("%s Both queues empty, returning silent fallback", qm.station.LogPrefix)
	return "", false
}

func (qm *QueueManager) popPlaylist() (string, bool) {
	if len(qm.station.PlaylistQueue) == 0 {
		return "", false
	}

	var idx int
	if qm.station.Config.Random {
		idx = rand.Intn(len(qm.station.PlaylistQueue))
	}

	file := qm.station.PlaylistQueue[idx]
	basename := filepath.Base(file)

	if qm.station.Config.Unique && (qm.station.PlayedSet[basename] || qm.station.PlayedSet[file]) {
		qm.station.PlaylistQueue = append(qm.station.PlaylistQueue[:idx], qm.station.PlaylistQueue[idx+1:]...)
		if len(qm.station.PlaylistQueue) == 0 {
			qm.station.PlayedSet = make(map[string]bool)
		}
		return qm.popPlaylist()
	}

	qm.station.PlaylistQueue = append(qm.station.PlaylistQueue[:idx], qm.station.PlaylistQueue[idx+1:]...)
	qm.station.PlayedSet[basename] = true
	qm.station.PlayedSet[file] = true

	log.Printf("%s Pop from PLAYLIST queue: %s (remaining: %d)",
		qm.station.LogPrefix, file, len(qm.station.PlaylistQueue))
	return file, false
}

func (qm *QueueManager) QueueLengths() (insertLen int, playlistLen int) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	return len(qm.station.InsertQueue), len(qm.station.PlaylistQueue)
}

func (qm *QueueManager) PeekNextFile() (string, bool) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if len(qm.station.InsertQueue) > 0 {
		return qm.station.InsertQueue[0], true
	}
	if len(qm.station.PlaylistQueue) > 0 {
		return qm.station.PlaylistQueue[0], false
	}
	return "", false
}

func (qm *QueueManager) PushPlaylistFront(files []string) {
	qm.mu.Lock()
	qm.station.PlaylistQueue = append(files, qm.station.PlaylistQueue...)
	log.Printf("%s Playlist queue +%d files to FRONT (total: %d)", qm.station.LogPrefix, len(files), len(qm.station.PlaylistQueue))
	qm.mu.Unlock()
	qm.Save()
}

func (qm *QueueManager) HasWork() bool {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	return len(qm.station.InsertQueue) > 0 || len(qm.station.PlaylistQueue) > 0
}

func (qm *QueueManager) ClearQueue(queueType string) int {
	qm.mu.Lock()

	removed := 0
	switch queueType {
	case "insert":
		removed = len(qm.station.InsertQueue)
		qm.station.InsertQueue = make([]string, 0)
	case "playlist":
		removed = len(qm.station.PlaylistQueue)
		qm.station.PlaylistQueue = make([]string, 0)
	case "all":
		removed = len(qm.station.InsertQueue) + len(qm.station.PlaylistQueue)
		qm.station.InsertQueue = make([]string, 0)
		qm.station.PlaylistQueue = make([]string, 0)
	}
	log.Printf("%s Queue %s cleared (%d removed)", qm.station.LogPrefix, queueType, removed)
	qm.mu.Unlock()
	qm.Save()
	return removed
}

func (qm *QueueManager) RemoveFromQueue(queueType string, filename string) bool {
	qm.mu.Lock()

	var queue *[]string
	var label string
	switch queueType {
	case "insert":
		queue = &qm.station.InsertQueue
		label = "INSERT"
	case "playlist":
		queue = &qm.station.PlaylistQueue
		label = "PLAYLIST"
	default:
		qm.mu.Unlock()
		return false
	}

	for i, f := range *queue {
		if f == filename || filepath.Base(f) == filepath.Base(filename) {
			*queue = append((*queue)[:i], (*queue)[i+1:]...)
			log.Printf("%s Removed from %s queue: %s", qm.station.LogPrefix, label, filename)
			qm.mu.Unlock()
			qm.Save()
			return true
		}
	}

	log.Printf("%s File not found in %s queue: %s", qm.station.LogPrefix, label, filename)
	qm.mu.Unlock()
	return false
}
