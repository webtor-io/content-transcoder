package services

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
)

var (
	re2 = regexp.MustCompile(`[asv][0-9]+(\-[0-9]+)?(\-[0-9]+)?\.[0-9a-z]{2,4}`)
	re3 = regexp.MustCompile(`(ts|vtt|\,)$`)
)

func validatePlaylist(b []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(b))
	i := 0
	for scanner.Scan() {
		text := scanner.Text()
		i++
		if text == "#EXT-X-ENDLIST" {
			return true
		}
		if i > 5 && !re3.Match([]byte(text)) {
			return false
		}
	}
	if i < 5 {
		return false
	}
	return true
}

func enrichPlaylist(b []byte, w http.ResponseWriter, r *http.Request) {
	var sb strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		text := scanner.Text()
		if r.URL.RawQuery != "" {
			text = re2.ReplaceAllString(text, "$0?"+r.URL.RawQuery)
		}
		if text == "#EXT-X-MEDIA-SEQUENCE:0" {
			sb.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT")
			sb.WriteRune('\n')
		}
		sb.WriteString(text)
		sb.WriteRune('\n')
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%v", sb.Len()))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Write([]byte(sb.String()))

	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
}

func enrichPlaylistHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		if !strings.HasSuffix(r.URL.Path, ".m3u8") {
			next.ServeHTTP(w, r)
			return
		}

		wi := NewBufferedResponseWrtier(w)

		r.Header.Del("Range")

		next.ServeHTTP(wi, r)

		if wi.statusCode != http.StatusOK {
			w.WriteHeader(wi.statusCode)
			return
		}

		b := wi.GetBufferedBytes()

		if p != "/index.m3u8" && !validatePlaylist(b) {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			enrichPlaylist(b, w, r)
		}
	})
}
