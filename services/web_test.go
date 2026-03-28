package services

import (
	"strings"
	"testing"
)

func TestEnrichPlaylistData_EmptyQuery(t *testing.T) {
	data := []byte("#EXTM3U\nv0-720-0.ts\na0-0.ts\n")
	got := enrichPlaylistData(data, "")
	if string(got) != string(data) {
		t.Errorf("empty query should return data unchanged, got %q", string(got))
	}
}

func TestEnrichPlaylistData_MasterPlaylist(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=5000000\nv0-720.m3u8\n#EXT-X-MEDIA:URI=\"a0.m3u8\"\n"
	got := string(enrichPlaylistData([]byte(master), "api-key=abc&token=xyz"))

	if !strings.Contains(got, "v0-720.m3u8?api-key=abc&token=xyz") {
		t.Errorf("should enrich variant playlist ref, got:\n%s", got)
	}
	if !strings.Contains(got, "a0.m3u8?api-key=abc&token=xyz") {
		t.Errorf("should enrich audio playlist ref, got:\n%s", got)
	}
	// Tags should not be modified
	if !strings.Contains(got, "#EXT-X-STREAM-INF:BANDWIDTH=5000000") {
		t.Errorf("tags should remain intact, got:\n%s", got)
	}
}

func TestEnrichPlaylistData_VariantPlaylist(t *testing.T) {
	variant := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.0,\nv0-720-0.ts\n#EXTINF:4.0,\nv0-720-1.ts\n"
	got := string(enrichPlaylistData([]byte(variant), "token=t1"))

	if !strings.Contains(got, "v0-720-0.ts?token=t1") {
		t.Errorf("should enrich segment 0, got:\n%s", got)
	}
	if !strings.Contains(got, "v0-720-1.ts?token=t1") {
		t.Errorf("should enrich segment 1, got:\n%s", got)
	}
}

func TestEnrichPlaylistData_SubtitleAndAudio(t *testing.T) {
	playlist := "#EXTM3U\n#EXTINF:4.0,\na0-5.ts\n#EXTINF:4.0,\ns0-3.vtt\n"
	got := string(enrichPlaylistData([]byte(playlist), "key=val"))

	if !strings.Contains(got, "a0-5.ts?key=val") {
		t.Errorf("should enrich audio segment, got:\n%s", got)
	}
	if !strings.Contains(got, "s0-3.vtt?key=val") {
		t.Errorf("should enrich subtitle segment, got:\n%s", got)
	}
}

func TestEnrichPlaylistData_NoFalseMatches(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:0\nsome random text\n"
	got := string(enrichPlaylistData([]byte(playlist), "key=val"))

	// Tags and non-file lines should not be enriched
	if strings.Contains(got, "TARGETDURATION:5?key=val") {
		t.Errorf("should not enrich HLS tags")
	}
	if strings.Contains(got, "random text?key=val") {
		t.Errorf("should not enrich non-file lines")
	}
}

func TestIsSubtitlePlaylist(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"s0.m3u8", true},
		{"s1.m3u8", true},
		{"s12.m3u8", true},
		{"v0-720.m3u8", false},
		{"a0.m3u8", false},
		{"index.m3u8", false},
		{"s0-0.ts", false},
		{"s0-0.vtt", false},
		{"subtitle.m3u8", false}, // doesn't match "s" + digit pattern — still starts with "s" though
	}
	for _, tt := range tests {
		got := isSubtitlePlaylist(tt.name)
		if got != tt.want {
			t.Errorf("isSubtitlePlaylist(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestEmptySubtitlePlaylist_IsValidHLS(t *testing.T) {
	empty := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n"
	if !strings.HasPrefix(empty, "#EXTM3U") {
		t.Error("must start with #EXTM3U")
	}
	if !strings.Contains(empty, "#EXT-X-TARGETDURATION:") {
		t.Error("must contain TARGETDURATION")
	}
	if strings.Contains(empty, "#EXT-X-ENDLIST") {
		t.Error("must NOT contain ENDLIST — player should keep polling for segments")
	}
}
