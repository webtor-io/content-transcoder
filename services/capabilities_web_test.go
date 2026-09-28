package services

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// capabilitiesWeb is a Web behind the real router whose capability comes
// from src (nil: no HLS builder at all).
func capabilitiesWeb(t *testing.T, src *passthroughCapabilitySource) *Web {
	t.Helper()
	sm := NewSessionManager(NewRunManager())
	t.Cleanup(sm.CloseAll)
	web := &Web{sessionManager: sm, touchMap: NewTouchMap()}
	if src != nil {
		web.hlsBuilder = &HLSBuilder{passthrough: src}
	}
	web.buildHandler()
	return web
}

func getCapabilities(web *Web, method string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	web.handler.ServeHTTP(w, httptest.NewRequest(method, "/capabilities", nil))
	return w
}

// An empty capability is an empty list, never null: a client reading the
// key as "the transcoder answered" must see an answer that says "none".
func TestCapabilities_EmptyIsAnEmptyList(t *testing.T) {
	buildableHEVC(t)
	for name, web := range map[string]*Web{
		"flag empty":     capabilitiesWeb(t, newPassthroughCapabilitySource("", "")),
		"no HLS builder": capabilitiesWeb(t, nil),
		"nothing usable": capabilitiesWeb(t, newPassthroughCapabilitySource("av1", "")),
	} {
		w := getCapabilities(web, http.MethodGet)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, w.Code)
		}
		if got, want := w.Body.String(), "{\"passthrough_video_codecs\":[]}\n"; got != want {
			t.Errorf("%s: body %q, want %q", name, got, want)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type %q", name, ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q", name, cc)
		}
		if acao := w.Header().Get("Access-Control-Allow-Origin"); acao != "" {
			t.Errorf("%s: CORS header %q on a service-only endpoint", name, acao)
		}
	}
}

func TestCapabilities_Hevc(t *testing.T) {
	buildableHEVC(t)
	w := getCapabilities(capabilitiesWeb(t, newPassthroughCapabilitySource("hevc", "")), http.MethodGet)
	if got, want := w.Body.String(), "{\"passthrough_video_codecs\":[\"hevc\"]}\n"; w.Code != 200 || got != want {
		t.Errorf("status %d body %q, want 200 %q", w.Code, got, want)
	}
}

// The answer is the one a session opened now gets: the file is re-read
// when it changes, with no new Web and no restart.
func TestCapabilities_FollowsTheFile(t *testing.T) {
	buildableHEVC(t)
	file := filepath.Join(t.TempDir(), "codecs")
	t0 := time.Now().Add(-time.Hour)
	writeCapability(t, file, "", t0)
	web := capabilitiesWeb(t, newPassthroughCapabilitySource("", file))
	for i, step := range []struct{ content, want string }{
		{"", `{"passthrough_video_codecs":[]}`},
		{"hevc\n", `{"passthrough_video_codecs":["hevc"]}`},
		{"", `{"passthrough_video_codecs":[]}`},
	} {
		writeCapability(t, file, step.content, t0.Add(time.Duration(i)*time.Minute))
		if got := getCapabilities(web, http.MethodGet).Body.String(); got != step.want+"\n" {
			t.Errorf("step %d (file %q): %q, want %q", i, step.content, got, step.want)
		}
	}
}

func TestCapabilities_OnlyGetAndHead(t *testing.T) {
	buildableHEVC(t)
	web := capabilitiesWeb(t, newPassthroughCapabilitySource("hevc", ""))
	if w := getCapabilities(web, http.MethodHead); w.Code != http.StatusOK {
		t.Errorf("HEAD: %d", w.Code)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := getCapabilities(web, m)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", m, w.Code)
		}
		if a := w.Header().Get("Allow"); a != "GET, HEAD" {
			t.Errorf("%s: Allow %q", m, a)
		}
	}
}

// sumCounter adds every series of a counter family on the default gatherer.
func sumCounter(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
	}
	return sum
}

func familyExists(t *testing.T, name string) bool {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() == name {
			return true
		}
	}
	return false
}

// Asking is free for the transcoder: no session, no route counted (the
// route metric is the share of real sessions), no touch of an output dir.
func TestCapabilities_NoSessionSideEffects(t *testing.T) {
	buildableHEVC(t)
	web := capabilitiesWeb(t, newPassthroughCapabilitySource("hevc", ""))
	routes := sumCounter(t, "transcoder_video_route_total")
	sessions := sumCounter(t, "transcoder_sessions_total")
	if !familyExists(t, "transcoder_video_route_total") || !familyExists(t, "transcoder_sessions_total") {
		t.Fatal("a watched metric is not registered under that name: the deltas below would be vacuous")
	}
	for i := 0; i < 3; i++ {
		getCapabilities(web, http.MethodGet)
	}
	web.sessionManager.mu.Lock()
	n := len(web.sessionManager.sessions)
	web.sessionManager.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions after asking", n)
	}
	if d := sumCounter(t, "transcoder_video_route_total") - routes; d != 0 {
		t.Errorf("video_route_total moved by %v", d)
	}
	if d := sumCounter(t, "transcoder_sessions_total") - sessions; d != 0 {
		t.Errorf("sessions_total moved by %v", d)
	}
}
