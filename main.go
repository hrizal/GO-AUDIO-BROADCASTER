package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/streamer/api"
	"github.com/streamer/station"
	"github.com/streamer/types"
)

const (
	AppName       = "Go Audio Broadcaster"
	AppVersion    = "1.0.0"
	StationConfig = "station.cfg"
)

func main() {
	lockFile, err := os.OpenFile("/tmp/streamer.lock", os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		fmt.Printf("Failed to open lock file: %v\n", err)
		os.Exit(1)
	}
	// Keep the file open to hold the lock
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("Streamer is already running. Exiting.")
		if sl, err := syslog.New(syslog.LOG_WARNING|syslog.LOG_DAEMON, "streamer"); err == nil {
			sl.Warning("Streamer is already running. Exiting.")
			sl.Close()
		}
		os.Exit(1)
	}

	port := flag.Int("port", 8080, "HTTP server port")
	outputDir := flag.String("output", "./output", "Default HLS output directory")
	silentDir := flag.String("silent", "./silent", "Directory containing silent_5s.mp3")
	autoStation := flag.String("station", "", "Create a station with this ID on startup")
	configFile := flag.String("config", StationConfig, "Station config file path")
	logFile := flag.String("log", "", "Log file path (empty = stdout)")
	shortcuts := flag.Bool("shortcuts", true, "Enable port 80 shortcuts (http://ip/station_id/)")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	log.SetPrefix("[Streamer] ")

	if *logFile == "" {
		*logFile = "streamer.log"
	}

	f, err := os.OpenFile(*logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Warning: cannot open log file %s: %v (logging to stdout only)", *logFile, err)
	} else {
		log.SetOutput(io.MultiWriter(os.Stdout, f))
		log.Printf("Logging to file: %s", *logFile)
	}

	absOutput, err := filepath.Abs(*outputDir)
	if err != nil {
		log.Fatalf("Failed to resolve output path: %v", err)
	}
	absSilent, err := filepath.Abs(*silentDir)
	if err != nil {
		log.Fatalf("Failed to resolve silent path: %v", err)
	}
	absConfig := *configFile
	if !filepath.IsAbs(absConfig) {
		absConfig, err = filepath.Abs(absConfig)
		if err != nil {
			log.Fatalf("Failed to resolve config path: %v", err)
		}
	}

	for _, dir := range []string{absOutput, absSilent} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("Failed to create directory %s: %v", dir, err)
		}
	}

	log.Printf("=== %s v%s ===", AppName, AppVersion)
	log.Printf("Config file: %s", absConfig)
	log.Printf("Default output directory: %s", absOutput)
	log.Printf("Silent directory: %s", absSilent)
	log.Printf("Serving on port: %d", *port)

	manager := station.NewManager(absSilent, absOutput)
	manager.SetConfigPath(absConfig)

	// Load stations from config file
	stationsLoaded := false

	if entries, err := loadStationIDs(absConfig); err == nil && len(entries) > 0 {
		log.Printf("Loaded %d station(s) from %s", len(entries), absConfig)
		for _, entry := range entries {
			runner, err := manager.CreateStationFromEntry(entry)
			if err != nil {
				log.Printf("Failed to create station %s: %v", entry.ID, err)
				continue
			}

			log.Printf("Station created: %s (output: %s) playlist: %s config: random=%v loop=%v unique=%v",
				runner.Station.ID, runner.Station.OutputDir,
				entry.PlaylistDir,
				entry.Config.Random, entry.Config.Loop, entry.Config.Unique)
			stationsLoaded = true
		}
	}

	// Fallback: -station flag
	if !stationsLoaded && *autoStation != "" {
		runner, err := manager.CreateStation(*autoStation)
		if err != nil {
			log.Fatalf("Failed to create station %s: %v", *autoStation, err)
		}
		log.Printf("Station created from -station flag: %s (output: %s)", runner.Station.ID, runner.Station.OutputDir)
		stationsLoaded = true
	}

	// Last fallback: default station
	if !stationsLoaded {
		runner, err := manager.CreateStation("default")
		if err != nil {
			log.Fatalf("Failed to create default station: %v", err)
		}
		log.Printf("Created default station: %s (output: %s)", runner.Station.ID, runner.Station.OutputDir)
	}

	// HTTP server
	apiHandler := api.NewHandler(manager, absOutput)
	mux := http.NewServeMux()
	apiHandler.RegisterRoutes(mux)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","app":"` + AppName + `","version":"` + AppVersion + `","stations":`))
		fmt.Fprintf(w, `%d}`, manager.StationCount())
	})

	mux.HandleFunc("/station/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		stationID := r.FormValue("station_id")
		if stationID == "" {
			http.Error(w, "Missing station_id", http.StatusBadRequest)
			return
		}

		entries, err := loadStationIDs(absConfig)
		if err != nil {
			http.Error(w, "Failed to read config", http.StatusInternalServerError)
			return
		}

		var newConfig *types.StationConfigEntry
		for _, e := range entries {
			if e.ID == stationID {
				newConfig = &e
				break
			}
		}

		if newConfig == nil {
			http.Error(w, "Station not found in config", http.StatusNotFound)
			return
		}

		st, exists := manager.GetStation(stationID)
		if !exists {
			http.Error(w, "Station not active", http.StatusNotFound)
			return
		}

		// Update config
		st.Station.Config = newConfig.Config
		
		// Soft Reset Audio Engine
		st.Encoder.Reset()

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","message":"Station config updated and audio engine restarted"}`))
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutdown signal received. Stopping all stations...")
		manager.StopAll()
		server.Close()
	}()

	log.Printf("HTTP server listening on :%d", *port)
	log.Printf("API endpoints:")
	log.Printf("  GET  /status?station_id=xxx - Station status")
	log.Printf("  POST /inject                - Inject files")
	log.Printf("  POST /station/create        - Create station")
	log.Printf("  POST /station/remove        - Remove station")
	log.Printf("  POST /station/update        - Update config & soft restart")
	log.Printf("  POST /queue/clear           - Clear queue")
	log.Printf("  POST /queue/remove          - Remove file from queue")
	log.Printf("  GET  /hls/{id}/{variant}/master.m3u8 - HLS")
	log.Printf("  GET  /health                - Health check")
	log.Printf("---")

	// Optional Port 80 Listener (Shortcuts)
	if *shortcuts {
		go func() {
			p80Mux := http.NewServeMux()
			apiHandler.RegisterPort80Routes(p80Mux)
			p80Server := &http.Server{
				Addr:    ":80",
				Handler: p80Mux,
			}
			
			log.Printf("Attempting to start shortcut listener on port 80...")
			if err := p80Server.ListenAndServe(); err != nil {
				log.Printf("[Warning] Port 80 listener could not start: %v (This is normal if port 80 is in use or no permission)", err)
			} else {
				log.Printf("[Success] Shortcut listener active on port 80 (http://yourip/station_id/)")
			}
		}()
	}

	if err := server.ListenAndServe(); err != nil {
		log.Printf("Server stopped: %v", err)
	}

	log.Println("Go Audio Broadcaster shutdown complete.")
}

// loadStationIDs reads station config file.
// Format per line: station_id  [key=value ...]
// Supported keys: random, loop, unique (bool), output (path)
// Example:
//   radio1  output=/var/www/hls/radio1  random=false  loop=true  unique=true
//   radio2 output=/var/www/hls/radio2  random=true   loop=true  unique=true
func loadStationIDs(path string) ([]types.StationConfigEntry, error) {
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
		var playlistDir string

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
			case "opus128":
				cfg.Opus128 = boolVal
			case "mp3":
				cfg.MP3 = boolVal
			case "output":
				if filepath.IsAbs(val) {
					outputDir = val
				} else {
					absVal, err := filepath.Abs(val)
					if err == nil {
						outputDir = absVal
					} else {
						outputDir = val
					}
				}
			case "playlist":
				if filepath.IsAbs(val) {
					playlistDir = val
				} else {
					absVal, err := filepath.Abs(val)
					if err == nil {
						playlistDir = absVal
					} else {
						playlistDir = val
					}
				}
			case "crossfade":
				if n, err := strconv.Atoi(val); err == nil {
					cfg.Crossfade = n
				}
			case "rtmp":
				cfg.RTMP = val
				log.Printf("[Config] Station %s: Found RTMP=%s", stationID, val)
			case "logo":
				cfg.Logo = val
				log.Printf("[Config] Station %s: Found Logo=%s", stationID, val)
			case "video_loop":
				cfg.VideoLoop = val
				log.Printf("[Config] Station %s: Found VideoLoop=%s", stationID, val)
			case "background", "background_image":
				cfg.BackgroundImage = val
				log.Printf("[Config] Station %s: Found BackgroundImage=%s", stationID, val)
			case "display_text":
				cfg.DisplayText = val
				log.Printf("[Config] Station %s: Found DisplayText=%s", stationID, val)
			}
		}

		entries = append(entries, types.StationConfigEntry{
			ID:         stationID,
			Config:     cfg,
			OutputDir:  outputDir,
			PlaylistDir: playlistDir,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading %s: %w", path, err)
	}

	return entries, nil
}
