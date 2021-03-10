package services

import "net/http"

func waitHandler(h http.Handler, wa *Waiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		select {
		case <-wa.Wait(r.Context(), r.URL.Path):
		case <-ctx.Done():
			if ctx.Err() != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			break
		}
		h.ServeHTTP(w, r)
	})
}
