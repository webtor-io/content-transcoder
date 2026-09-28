package services

import (
	"encoding/json"
	"net/http"
)

// capabilitiesResponse answers "which source video codecs does this
// transcoder hand to players as they are" for a session opened now:
// passthroughCapability, re-read from its file exactly as POST /session
// reads it. The route of a session is still decided per session
// (videoRouteFor, from the source and the client's declaration); this only
// tells a client whether passthrough exists here at all.
type capabilitiesResponse struct {
	// PassthroughVideoCodecs is sorted and never null: [] passes none.
	PassthroughVideoCodecs []string `json:"passthrough_video_codecs"`
}

// capabilitiesHandler handles GET /capabilities. It is for services (web-ui
// asks it before promising 4K HEVC), not browsers: no CORS headers. It
// touches no session, run or metric.
// @Summary Transcoder capabilities
// @Description Which source video codecs a session opened now hands to the player as they are (HEVC passthrough). Re-read from the capability file like POST /session; the route itself is decided per session.
// @Tags capabilities
// @Produce json
// @Success 200 {object} capabilitiesResponse
// @Failure 405 {string} string "Method not allowed"
// @Router /capabilities [get]
func (s *Web) capabilitiesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var c passthroughCapability
	if s.hlsBuilder != nil {
		c = s.hlsBuilder.PassthroughCapability()
	}
	b, err := json.Marshal(capabilitiesResponse{PassthroughVideoCodecs: c.list()})
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(b, '\n'))
}
