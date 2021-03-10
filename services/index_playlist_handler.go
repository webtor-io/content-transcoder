package services

import (
	"net/http"

	log "github.com/sirupsen/logrus"
)

func indexPlaylistHandler(h *HLSParser) http.Handler {
	mp := ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mp == "" {
			hls, err := h.Get()
			if err != nil {
				log.WithError(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
			mp = hls.MakeMasterPlaylist()
		}
		w.Write([]byte(mp))
	})
}
