package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Channel struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Config struct {
	M3U8Path   string `json:"m3u8_path"`
	RtmpURL    string `json:"rtmp_url"`
	FlvURL     string `json:"flv_url"`
	HlsURL     string `json:"hls_url"`
	HTTPAddr   string `json:"http_addr"`
	FFmpegPath string `json:"ffmpeg_path"`
}

type Server struct {
	cfg           Config
	mu            sync.Mutex
	channels      []Channel
	activeCmd     *exec.Cmd
	activeChannel string
	activeSince   time.Time
}

func readConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()

	var cfg Config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseM3U8(path string) ([]Channel, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var channels []Channel
	var currentName string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "#EXTINF:") {
			comma := strings.Index(line, ",")
			if comma >= 0 && comma+1 < len(line) {
				currentName = strings.TrimSpace(line[comma+1:])
			} else {
				currentName = strings.TrimSpace(line)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if currentName == "" {
			continue
		}
		channels = append(channels, Channel{Name: currentName, URL: line})
		currentName = ""
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return channels, nil
}

func (s *Server) loadChannels() error {
	channels, err := parseM3U8(s.cfg.M3U8Path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.channels = channels
	s.mu.Unlock()
	return nil
}

func (s *Server) stopFFmpegLocked() {
	if s.activeCmd == nil || s.activeCmd.Process == nil {
		return
	}
	_ = s.activeCmd.Process.Kill()
	_ = s.activeCmd.Wait()
	s.activeCmd = nil
	s.activeChannel = ""
}

func (s *Server) stopFFmpeg() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopFFmpegLocked()
}

func (s *Server) startFFmpeg(url, channelName string) error {
	s.mu.Lock()
	s.stopFFmpegLocked()

	cmd := exec.Command(s.cfg.FFmpegPath,
		"-i", url,
		"-c:v", "libx264",
		"-b:v", "2000k",
		"-preset", "medium",
		"-c:a", "aac",
		"-b:a", "128k",
		"-f", "flv",
		s.cfg.RtmpURL,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return err
	}

	s.activeCmd = cmd
	s.activeChannel = channelName
	s.activeSince = time.Now()
	s.mu.Unlock()

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("ffmpeg exited: %v", err)
		}
		// clear if this process is still the active one
		s.mu.Lock()
		if s.activeCmd == cmd {
			s.activeCmd = nil
			s.activeChannel = ""
		}
		s.mu.Unlock()
	}()

	return nil
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	s.mu.Lock()
	channels := make([]Channel, len(s.channels))
	copy(channels, s.channels)
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"channels": channels})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	s.mu.Lock()
	status := map[string]any{
		"active_channel": s.activeChannel,
		"flv_url":        s.cfg.FlvURL,
		"hls_url":        s.cfg.HlsURL,
		"rtmp_url":       s.cfg.RtmpURL,
		"active_since":   s.activeSince.Format(time.RFC3339),
	}
	s.mu.Unlock()
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	var selected *Channel
	s.mu.Lock()
	for _, ch := range s.channels {
		if ch.Name == req.Name {
			selected = &Channel{Name: ch.Name, URL: ch.URL}
			break
		}
	}
	s.mu.Unlock()

	if selected == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	if err := s.startFFmpeg(selected.URL, selected.Name); err != nil {
		log.Printf("failed to start ffmpeg: %v", err)
		http.Error(w, "failed to start transcoding", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"active_channel": selected.Name,
		"flv_url":        s.cfg.FlvURL,
		"hls_url":        s.cfg.HlsURL,
	})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.loadChannels(); err != nil {
		http.Error(w, fmt.Sprintf("reload failed: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"status": "reloaded"})
}

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config.json"
	}
	if !filepath.IsAbs(cfgPath) {
		cfgPath = filepath.Join(".", cfgPath)
	}

	cfg, err := readConfig(cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.M3U8Path == "" {
		cfg.M3U8Path = "iptv.m3u8"
	}
	if cfg.RtmpURL == "" || cfg.FlvURL == "" || cfg.HlsURL == "" {
		log.Fatal("rtmp_url, flv_url and hls_url are required in config.json")
	}

	server := &Server{cfg: cfg}
	if err := server.loadChannels(); err != nil {
		log.Fatalf("load channels: %v", err)
	}

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/api/channels", server.handleChannels)
	http.HandleFunc("/api/play", server.handlePlay)
	http.HandleFunc("/api/status", server.handleStatus)
	http.HandleFunc("/api/reload", server.handleReload)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.FileServer(http.Dir("static")).ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, "static/index.html")
	})

	log.Printf("starting server on %s", cfg.HTTPAddr)
	log.Fatal(http.ListenAndServe(cfg.HTTPAddr, nil))
}
