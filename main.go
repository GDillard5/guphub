package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

// Config holds server configuration
type Config struct {
	Port         string
	VideoDir     string
	ThumbnailDir string
	StaticDir    string
	TemplatesDir string
	DataFile     string
	TagsFile     string
	Password     string
}

// Video represents a video with metadata
type Video struct {
	ID        string   `json:"id"`
	Filename  string   `json:"filename"`
	Thumbnail string   `json:"thumbnail"`
	Title     string   `json:"title"`
	Game      string   `json:"game,omitempty"`
	People    []string `json:"people,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// VideoStore manages video metadata
type VideoStore struct {
	mu       sync.RWMutex
	videos   []Video
	dataFile string
}

func NewVideoStore(dataFile string) (*VideoStore, error) {
	vs := &VideoStore{videos: []Video{}, dataFile: dataFile}
	if err := vs.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return vs, nil
}

func (vs *VideoStore) load() error {
	data, err := os.ReadFile(vs.dataFile)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &vs.videos)
}

func (vs *VideoStore) save() error {
	data, err := json.MarshalIndent(vs.videos, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(vs.dataFile, data, 0644)
}

func (vs *VideoStore) Add(v Video) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.videos = append([]Video{v}, vs.videos...) // prepend (newest first)
	return vs.save()
}

func (vs *VideoStore) All() []Video {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.videos
}

// TagOptions holds available tags
type TagOptions struct {
	Games  []string `json:"games"`
	People []string `json:"people"`
}

func LoadTagOptions(path string) (TagOptions, error) {
	var opts TagOptions
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return defaults if file doesn't exist
			log.Printf("tags.json not found, using defaults")
			return TagOptions{
				Games:  []string{"Minecraft", "Valorant", "CS2", "League of Legends", "Fortnite", "Apex Legends"},
				People: []string{"Alice", "Bob", "Charlie", "Diana", "Eve", "Frank"},
			}, nil
		}
		return opts, err
	}
	err = json.Unmarshal(data, &opts)
	return opts, err
}

// Server holds dependencies
type Server struct {
	config     Config
	templates  *template.Template
	sessions   *SessionStore
	videos     *VideoStore
	tagOptions TagOptions
}

// SessionStore manages valid session tokens
type SessionStore struct {
	mu     sync.RWMutex
	tokens map[string]time.Time
}

func NewSessionStore() *SessionStore {
	s := &SessionStore{tokens: make(map[string]time.Time)}
	go s.cleanup()
	return s
}

func (s *SessionStore) Create() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[token] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	return token
}

func (s *SessionStore) Valid(token string) bool {
	s.mu.RLock()
	expiry, ok := s.tokens[token]
	s.mu.RUnlock()
	return ok && time.Now().Before(expiry)
}

func (s *SessionStore) cleanup() {
	for {
		time.Sleep(time.Hour)
		s.mu.Lock()
		for token, expiry := range s.tokens {
			if time.Now().After(expiry) {
				delete(s.tokens, token)
			}
		}
		s.mu.Unlock()
	}
}

func NewServer(cfg Config) (*Server, error) {
	tmpl, err := template.ParseGlob(filepath.Join(cfg.TemplatesDir, "*.html"))
	if err != nil {
		return nil, err
	}
	tmpl, err = tmpl.ParseGlob(filepath.Join(cfg.TemplatesDir, "partials", "*.html"))
	if err != nil {
		return nil, err
	}

	videos, err := NewVideoStore(cfg.DataFile)
	if err != nil {
		return nil, err
	}

	tags, err := LoadTagOptions(cfg.TagsFile)
	if err != nil {
		return nil, err
	}

	return &Server{
		config:     cfg,
		templates:  tmpl,
		sessions:   NewSessionStore(),
		videos:     videos,
		tagOptions: tags,
	}, nil
}

func main() {
	cfg := Config{
		Port:         ":" + getEnv("PORT", "8080"),
		VideoDir:     getEnv("VIDEO_DIR", "./videos"),
		ThumbnailDir: getEnv("THUMBNAIL_DIR", "./thumbnails"),
		StaticDir:    getEnv("STATIC_DIR", "./static"),
		TemplatesDir: getEnv("TEMPLATES_DIR", "./templates"),
		DataFile:     getEnv("DATA_FILE", "./data/videos.json"),
		TagsFile:     getEnv("TAGS_FILE", "./data/tags.json"),
		Password:     getEnv("PASSWORD", "changeme"),
	}

	// Ensure directories exist
	os.MkdirAll(cfg.VideoDir, 0755)
	os.MkdirAll(cfg.ThumbnailDir, 0755)
	os.MkdirAll(filepath.Dir(cfg.DataFile), 0755)

	srv, err := NewServer(cfg)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()

	// Static files
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(cfg.StaticDir))))

	// Thumbnails
	mux.Handle("GET /thumbnails/", http.StripPrefix("/thumbnails/", http.FileServer(http.Dir(cfg.ThumbnailDir))))

	// Login
	mux.HandleFunc("GET /", srv.handleLogin)
	mux.HandleFunc("POST /login", srv.handleLoginSubmit)

	// Health check (for load balancers)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Protected routes
	mux.Handle("GET /home", srv.requireAuth(srv.handleHomeRedirect))
	mux.Handle("GET /videos", srv.requireAuth(srv.handleVideoList))
	mux.Handle("GET /upload", srv.requireAuth(srv.handleUploadPage))
	mux.Handle("POST /upload", srv.requireAuth(srv.handleUploadSubmit))
	mux.Handle("GET /videos/", srv.requireAuthHandler(srv.videoHandler()))
	mux.Handle("GET /partials/video-list", srv.requireAuth(srv.handleVideoListPartial))

	server := &http.Server{
		Addr:         cfg.Port,
		Handler:      mux,
		ReadTimeout:  0, // no timeout for large uploads
		WriteTimeout: 0, // no timeout for large uploads
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Server starting on http://localhost%s", cfg.Port)
	log.Fatal(server.ListenAndServe())
}

func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil || !s.sessions.Valid(cookie.Value) {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}

func (s *Server) requireAuthHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil || !s.sessions.Valid(cookie.Value) {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil && s.sessions.Valid(cookie.Value) {
		http.Redirect(w, r, "/home", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", nil)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	password := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(password), []byte(s.config.Password)) != 1 {
		w.Header().Set("HX-Retarget", "#error")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Incorrect password"))
		return
	}

	token := s.sessions.Create()
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
	})

	w.Header().Set("HX-Redirect", "/videos")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleHomeRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/videos", http.StatusSeeOther)
}

func (s *Server) videoHandler() http.Handler {
	fs := http.FileServer(http.Dir(s.config.VideoDir))
	return http.StripPrefix("/videos/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fs.ServeHTTP(w, r)
	}))
}

func (s *Server) handleVideoList(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Tags": s.tagOptions,
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "videos-content.html", data)
		return
	}
	s.render(w, "videos.html", data)
}

func (s *Server) handleVideoListPartial(w http.ResponseWriter, r *http.Request) {
	game := r.URL.Query().Get("game")
	person := r.URL.Query().Get("person")

	videos := s.videos.All()
	filtered := make([]Video, 0)

	for _, v := range videos {
		if game != "" && v.Game != game {
			continue
		}
		if person != "" {
			found := false
			for _, p := range v.People {
				if p == person {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		filtered = append(filtered, v)
	}

	s.render(w, "video-list.html", filtered)
}

func (s *Server) handleUploadPage(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "upload-content.html", s.tagOptions)
		return
	}
	s.render(w, "upload.html", s.tagOptions)
}

func (s *Server) handleUploadSubmit(w http.ResponseWriter, r *http.Request) {
	// Limit to 10GB
	r.Body = http.MaxBytesReader(w, r.Body, 10<<30)

	// Parse multipart form with 32MB memory buffer (rest goes to disk)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		log.Printf("parse form error: %v", err)
		http.Error(w, "File too large or failed to parse", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		log.Printf("form file error: %v", err)
		http.Error(w, "No video file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()

	title := r.FormValue("title")
	if title == "" {
		http.Error(w, "Title required", http.StatusBadRequest)
		return
	}

	game := r.FormValue("game")
	people := r.Form["people"]

	// Generate unique ID
	idBytes := make([]byte, 8)
	rand.Read(idBytes)
	id := hex.EncodeToString(idBytes)

	// Preserve file extension
	ext := filepath.Ext(header.Filename)
	filename := id + ext

	// Save file
	dstPath := filepath.Join(s.config.VideoDir, filename)
	dst, err := os.Create(dstPath)
	if err != nil {
		log.Printf("create file error: %v", err)
		http.Error(w, "Upload failed", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	written, err := io.Copy(dst, file)
	if err != nil {
		log.Printf("copy file error: %v", err)
		os.Remove(dstPath) // cleanup partial file
		http.Error(w, "Upload failed", http.StatusInternalServerError)
		return
	}
	log.Printf("uploaded %s (%d bytes)", filename, written)

	// Generate thumbnail using ffmpeg
	thumbnailName := id + ".jpg"
	thumbnailPath := filepath.Join(s.config.ThumbnailDir, thumbnailName)
	if err := generateThumbnail(dstPath, thumbnailPath); err != nil {
		log.Printf("thumbnail generation failed: %v", err)
		// Continue without thumbnail - not fatal
		thumbnailName = ""
	}

	// Save metadata
	video := Video{
		ID:        id,
		Filename:  filename,
		Thumbnail: thumbnailName,
		Title:     title,
		Game:      game,
		People:    people,
		CreatedAt: time.Now(),
	}

	if err := s.videos.Add(video); err != nil {
		log.Printf("save metadata error: %v", err)
		http.Error(w, "Upload failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/videos", http.StatusSeeOther)
}

// generateThumbnail creates a thumbnail from the first frame using ffmpeg
func generateThumbnail(videoPath, thumbnailPath string) error {
	cmd := exec.Command("ffmpeg",
		"-y",             // overwrite output
		"-i", videoPath,  // input file
		"-vf", "thumbnail,scale=320:-1", // select best frame, scale to 320px wide
		"-frames:v", "1", // output 1 frame
		thumbnailPath,
	)
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("ffmpeg error: %v, output: %s", err, string(output))
		return err
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
