package services

import (
	"io/ioutil"
	"net/http"

	log "github.com/sirupsen/logrus"
)

func indexPlaylistHandler(h *HLSParser, output string) http.Handler {
	mp := ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mp == "" {
			hls, err := h.Get()
			if err != nil {
				log.WithError(err).Error("Failed to generate index.m3u8")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mp = hls.MakeMasterPlaylist()
			err = ioutil.WriteFile(output+"/index.m3u8", []byte(mp), 0644)
			if err != nil {
				log.WithError(err).Error("Failed to write index.m3u8")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Write([]byte(mp))
	})
}
