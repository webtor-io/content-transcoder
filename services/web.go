package services

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	httpSwagger "github.com/swaggo/http-swagger/v2"
	"github.com/urfave/cli"

	_ "github.com/webtor-io/content-transcoder/docs"
	cp "github.com/webtor-io/content-prober/content-prober"
)

const (
	webHostFlag   = "host"
	webPortFlag   = "port"
	webPlayerFlag = "player"
)

func RegisterWebFlags(f []cli.Flag) []cli.Flag {
	return append(f, cli.StringFlag{
		Name:   webHostFlag + ", H",
		Usage:  "host",
		Value:  "",
		EnvVar: "WEB_HOST",
	}, cli.IntFlag{
		Name:   webPortFlag + ", P",
		Usage:  "port",
		Value:  8080,
		EnvVar: "WEB_PORT",
	}, cli.BoolFlag{
		Name:   webPlayerFlag,
		Usage:  "player",
		EnvVar: "PLAYER",
	})
}

type Web struct {
	host           string
	port           int
	player         bool
	output         string
	handler        http.Handler
	ln             net.Listener
	contentProbe   *ContentProbe
	hlsBuilder     *HLSBuilder
	sessionManager *SessionManager
	touchMap       *TouchMap
}

func NewWeb(c *cli.Context, contentProbe *ContentProbe, hlsBuilder *HLSBuilder, sessionManager *SessionManager, touchMap *TouchMap) *Web {
	we := &Web{
		host:           c.String(webHostFlag),
		port:           c.Int(webPortFlag),
		player:         c.Bool(webPlayerFlag),
		output:         c.String(OutputFlag),
		contentProbe:   contentProbe,
		hlsBuilder:     hlsBuilder,
		sessionManager: sessionManager,
		touchMap:       touchMap,
	}
	we.buildHandler()
	return we
}

func getSourceURL(r *http.Request) string {
	if r.Header.Get("X-Source-Url") != "" {
		return r.Header.Get("X-Source-Url")
	}
	raw := r.URL.RawQuery
	const prefix = "source_url="
	idx := strings.Index(raw, prefix)
	if idx >= 0 {
		val := raw[idx+len(prefix):]
		decoded, err := url.QueryUnescape(val)
		if err == nil {
			return decoded
		}
		return val
	}
	return ""
}

type PlayerData struct {
	SourceURL string
}

func (s *Web) playerHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles("player/index.html")
	if err != nil {
		http.Error(w, "unable to load template", http.StatusInternalServerError)
		return
	}
	err = tmpl.Execute(w, &PlayerData{
		SourceURL: getSourceURL(r),
	})
	if err != nil {
		http.Error(w, "unable to render template", http.StatusInternalServerError)
		return
	}
}

func getDuration(pr *cp.ProbeReply) float64 {
	if pr.GetFormat() != nil {
		d, err := strconv.ParseFloat(pr.GetFormat().GetDuration(), 64)
		if err == nil {
			return d
		}
	}
	return 0
}

func (s *Web) buildHandler() {
	mux := http.NewServeMux()
	if s.player {
		mux.HandleFunc("/player/", s.playerHandler)
	}

	// Session API routes
	mux.HandleFunc("/session", s.sessionCreateHandler)
	mux.HandleFunc("/session/", s.sessionRouter)

	// Swagger UI at /swagger/
	mux.Handle("/swagger/", httpSwagger.WrapHandler)

	s.handler = mux
}

func parseSegmentNumber(urlPath string) (int, error) {
	base := filepath.Base(urlPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	parts := strings.Split(base, "-")
	if len(parts) < 2 {
		return 0, fmt.Errorf("unexpected segment name: %s", urlPath)
	}
	return strconv.Atoi(parts[len(parts)-1])
}

func (s *Web) Serve() error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "failed to bind address")
	}
	s.ln = ln
	log.Infof("serving Web at %v", addr)
	if s.player {
		log.Info(fmt.Sprintf("player available at http://%v/player/", addr))
	}
	return http.Serve(ln, s.handler)
}

func (s *Web) Close() {
	log.Info("closing Web")
	defer func() {
		log.Info("Web closed")
	}()
	s.sessionManager.CloseAll()
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

// --- Session API handlers ---

type sessionCreateResponse struct {
	ID       string  `json:"id"`
	Duration float64 `json:"duration"`
}

// sessionCreateHandler handles POST /session?source_url=...
// @Summary Create transcoding session
// @Description Creates a new session, probes media, starts FFmpeg from position 0
// @Tags session
// @Produce json
// @Param source_url query string false "Source media URL (alternative to X-Source-Url header)"
// @Param X-Source-Url header string false "Source media URL (takes priority over query param)"
// @Success 200 {object} sessionCreateResponse
// @Failure 400 {string} string "Missing or invalid source_url"
// @Failure 500 {string} string "Internal error"
// @Router /session [post]
func (s *Web) sessionCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sourceURL := getSourceURL(r)
	if sourceURL == "" {
		http.Error(w, "missing source_url", http.StatusBadRequest)
		return
	}

	// Compute hash dir (same sharding as before)
	u, err := url.Parse(sourceURL)
	if err != nil {
		http.Error(w, "invalid source_url", http.StatusBadRequest)
		return
	}
	h := sha1.New()
	h.Write([]byte(u.Path))
	hash := hex.EncodeToString(h.Sum(nil))
	hashDir, err := GetDir(s.output, hash)
	if err != nil {
		http.Error(w, "failed to get output dir", http.StatusInternalServerError)
		return
	}

	if err := os.MkdirAll(hashDir, 0755); err != nil {
		http.Error(w, "failed to create output dir", http.StatusInternalServerError)
		return
	}

	// Touch hashDir so external cleanup knows it's active
	_, _ = s.touchMap.Touch(hashDir)

	// Probe media
	pr, err := s.contentProbe.Get(sourceURL, hashDir)
	if err != nil {
		log.WithError(err).Error("session: failed to probe media")
		http.Error(w, "failed to probe media", http.StatusInternalServerError)
		return
	}

	duration := getDuration(pr)
	hls := s.hlsBuilder.Build(sourceURL, pr)

	// Create session
	sess := s.sessionManager.Create(SessionConfig{
		SourceURL: sourceURL,
		HashDir:   hashDir,
		HLS:       hls,
		Duration:  duration,
	})

	// Create session directory and write master playlist
	if err := os.MkdirAll(sess.outputDir, 0755); err != nil {
		s.sessionManager.Close(sess.id)
		http.Error(w, "failed to create session dir", http.StatusInternalServerError)
		return
	}
	if err := hls.MakeMasterPlaylist(sess.outputDir); err != nil {
		s.sessionManager.Close(sess.id)
		http.Error(w, "failed to create master playlist", http.StatusInternalServerError)
		return
	}

	// Start FFmpeg from 0
	if err := sess.Start(0); err != nil {
		s.sessionManager.Close(sess.id)
		log.WithError(err).Error("session: failed to start ffmpeg")
		http.Error(w, "failed to start transcoding", http.StatusInternalServerError)
		return
	}

	resp, err := json.Marshal(sessionCreateResponse{
		ID:       sess.id,
		Duration: duration,
	})
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

// sessionRouter routes /session/{id}/... requests.
func (s *Web) sessionRouter(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Parse path: /session/{id}/...
	path := strings.TrimPrefix(r.URL.Path, "/session/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}

	sessionID := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	sess := s.sessionManager.Get(sessionID)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Update .touch file on hashDir so external cleanup knows content is active
	_, _ = s.touchMap.Touch(sess.hashDir)

	// Sanitize subPath — use only the base filename to prevent path traversal
	safeName := filepath.Base(subPath)

	switch {
	case subPath == "seek" && r.Method == http.MethodGet:
		s.sessionSeekOffsetHandler(w, r, sess)
	case subPath == "seek" && r.Method == http.MethodPost:
		s.sessionSeekHandler(w, r, sess)
	case subPath == "" && r.Method == http.MethodDelete:
		s.sessionCloseHandler(w, r, sess)
	case strings.HasSuffix(safeName, ".m3u8"):
		s.sessionPlaylistHandler(w, r, sess, safeName)
	case strings.HasSuffix(safeName, ".ts") || strings.HasSuffix(safeName, ".vtt"):
		s.sessionSegmentHandler(w, r, sess, safeName)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// sessionSeekHandler handles POST /session/{id}/seek?t=...
// @Summary Seek to position
// @Description Stops current FFmpeg run and starts new one from target position. Seek times are quantized to 30s boundaries.
// @Tags session
// @Produce json
// @Param sessionId path string true "Session ID"
// @Param t query number true "Target seek time in seconds"
// @Success 200 {object} map[string]bool
// @Failure 400 {string} string "Missing or invalid t parameter"
// @Failure 404 {string} string "Session not found"
// @Failure 500 {string} string "Seek failed"
// @Router /session/{sessionId}/seek [post]
func (s *Web) sessionSeekHandler(w http.ResponseWriter, r *http.Request, sess *Session) {
	tStr := r.URL.Query().Get("t")
	if tStr == "" {
		http.Error(w, "missing t parameter", http.StatusBadRequest)
		return
	}
	t, err := strconv.ParseFloat(tStr, 64)
	if err != nil {
		http.Error(w, "invalid t parameter", http.StatusBadRequest)
		return
	}

	if err := sess.Seek(t); err != nil {
		log.WithError(err).WithField("sessionID", sess.id).Error("session: seek failed")
		http.Error(w, "seek failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// sessionSeekOffsetHandler handles GET /session/{id}/seek
// @Summary Get current seek offset
// @Description Returns the current quantized seek position of the session
// @Tags session
// @Produce json
// @Param sessionId path string true "Session ID"
// @Success 200 {object} map[string]float64
// @Failure 404 {string} string "Session not found"
// @Router /session/{sessionId}/seek [get]
func (s *Web) sessionSeekOffsetHandler(w http.ResponseWriter, r *http.Request, sess *Session) {
	sess.Touch()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"offset":%.3f}`, sess.SeekTime())
}

// sessionCloseHandler handles DELETE /session/{id}
// @Summary Close session
// @Description Stops FFmpeg, releases shared run, removes session directory
// @Tags session
// @Produce json
// @Param sessionId path string true "Session ID"
// @Success 200 {object} map[string]bool
// @Failure 404 {string} string "Session not found"
// @Router /session/{sessionId} [delete]
func (s *Web) sessionCloseHandler(w http.ResponseWriter, r *http.Request, sess *Session) {
	s.sessionManager.Close(sess.id)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// sessionPlaylistHandler handles GET /session/{id}/{stream}.m3u8
// @Summary Get HLS playlist
// @Description Returns master playlist (index.m3u8) or variant EVENT playlist. Query params are appended to all file references for auth forwarding.
// @Tags session
// @Produce application/vnd.apple.mpegurl
// @Param sessionId path string true "Session ID"
// @Param stream path string true "Playlist name (index.m3u8, v0-720.m3u8, a0.m3u8, etc.)"
// @Success 200 {string} string "HLS playlist"
// @Failure 404 {string} string "Session or playlist not found"
// @Failure 504 {string} string "Timeout waiting for playlist"
// @Router /session/{sessionId}/{stream}.m3u8 [get]
func (s *Web) sessionPlaylistHandler(w http.ResponseWriter, r *http.Request, sess *Session, name string) {
	sess.Touch()

	var data []byte
	var err error

	if name == "index.m3u8" {
		// Serve master playlist directly from disk
		data, err = os.ReadFile(filepath.Join(sess.outputDir, "index.m3u8"))
		if err != nil {
			http.Error(w, "master playlist not found", http.StatusNotFound)
			return
		}
	} else {
		// Ensure FFmpeg is running (may have been released due to inactivity)
		if !sess.IsRunning() {
			if err := sess.EnsureRunning(); err != nil {
				log.WithError(err).WithField("sessionID", sess.id).Error("session: failed to restart for playlist")
			}
		}

		// For subtitle playlists (s*.m3u8): check if the playlist already
		// exists on disk. If not, use a short timeout and fall back to an
		// empty live playlist so subtitle issues don't block video playback.
		// Without #EXT-X-ENDLIST the player keeps polling, so if FFmpeg
		// eventually produces segments they will be picked up.
		if isSubtitlePlaylist(name) {
			data, err = sess.PlaylistForStream(name)
			if err != nil || len(data) == 0 || !isValidSessionPlaylist(data) {
				// Not ready yet — wait briefly on first attempt
				data, err = sess.WaitForPlaylist(r.Context(), name, 30*time.Second)
			}
			if err != nil || len(data) == 0 {
				if r.Context().Err() != nil {
					return
				}
				data = []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n")
			}
		} else {
			// Wait for variant playlist
			data, err = sess.WaitForPlaylist(r.Context(), name, 5*time.Minute)
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				log.WithError(err).WithFields(log.Fields{
					"sessionID": sess.id,
					"playlist":  name,
				}).Error("session: playlist timeout")
				http.Error(w, "playlist timeout", http.StatusGatewayTimeout)
				return
			}
		}
	}

	// Enrich: append query params (api-key, token, etc.) to all file
	// references so subsequent requests carry the same auth context.
	data = enrichPlaylistData(data, r.URL.RawQuery)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}

// sessionSegmentHandler handles GET /session/{id}/{segment}.ts|.vtt
// @Summary Get HLS segment
// @Description Returns a .ts or .vtt segment. Waits for file to appear if FFmpeg hasn't produced it yet. Auto-restarts FFmpeg if it was stopped.
// @Tags session
// @Produce video/mp2t
// @Param sessionId path string true "Session ID"
// @Param segment path string true "Segment filename (e.g., v0-720-0.ts, a0-5.ts)"
// @Success 200 {file} binary "Segment data"
// @Failure 404 {string} string "Session not found"
// @Failure 504 {string} string "Timeout waiting for segment"
// @Router /session/{sessionId}/{segment} [get]
func (s *Web) sessionSegmentHandler(w http.ResponseWriter, r *http.Request, sess *Session, filename string) {
	sess.Touch()

	// If FFmpeg is not running, auto-restart from the right position
	if !sess.IsRunning() {
		segNum, err := parseSegmentNumber("/" + filename)
		if err == nil {
			if err := sess.RestartForSegment(segNum); err != nil {
				log.WithError(err).WithField("sessionID", sess.id).Error("session: failed to restart for segment")
			}
		}
	}

	// Wait for the segment file to appear
	if err := sess.WaitForSegment(r.Context(), filename, 5*time.Minute); err != nil {
		if r.Context().Err() != nil {
			return
		}
		log.WithError(err).WithFields(log.Fields{
			"sessionID": sess.id,
			"segment":   filename,
		}).Error("session: segment timeout")
		http.Error(w, "segment timeout", http.StatusGatewayTimeout)
		return
	}

	// Serve the file
	http.ServeFile(w, r, sess.SegmentPath(filename))
}

// playlistFilePattern matches segment and playlist references in HLS playlists.
// E.g., "v0-720-5.ts", "a0-3.ts", "v0-720.m3u8", "a0.m3u8"
var playlistFilePattern = regexp.MustCompile(`[asv][0-9]+(-[0-9]+)?(-[0-9]+)?\.[0-9a-z]{2,4}`)

// enrichPlaylistData appends the request's query parameters to all segment
// and playlist references in an HLS playlist. In production, query params
// carry auth tokens (api-key, token) that must be forwarded to subsequent
// requests for segments and sub-playlists.
func enrichPlaylistData(data []byte, rawQuery string) []byte {
	if rawQuery == "" {
		return data
	}
	var sb strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		line = playlistFilePattern.ReplaceAllString(line, "$0?"+rawQuery)
		sb.WriteString(line)
		sb.WriteRune('\n')
	}
	return []byte(sb.String())
}

// isSubtitlePlaylist returns true if the playlist name corresponds to a
// subtitle stream (e.g. "s0.m3u8", "s1.m3u8"). Format: s{digit(s)}.m3u8
func isSubtitlePlaylist(name string) bool {
	if !strings.HasPrefix(name, "s") || !strings.HasSuffix(name, ".m3u8") {
		return false
	}
	mid := name[1 : len(name)-5] // between "s" and ".m3u8"
	if mid == "" {
		return false
	}
	for _, c := range mid {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}
