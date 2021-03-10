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

func enrichPlaylistHandler(h http.Handler) http.Handler {
	re := regexp.MustCompile(`\.m3u8$`)
	re2 := regexp.MustCompile(`[asv][0-9]+(\-[0-9]+)?\.[0-9a-z]{2,4}`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !re.MatchString(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}

		wi := NewBufferedResponseWrtier(w)

		h.ServeHTTP(wi, r)

		b := wi.GetBufferedBytes()

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
	})
}
