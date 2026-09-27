// Media server for the passthrough e2e: http.FileServer (Range support),
// logging every request with its Range header and the bytes it sent.
package main

import (
	"fmt"
	"strconv"
	"strings"
	"net/http"
	"os"
	"sync"
	"time"
)

// throttled writes at most rate bytes per second (a seeder is not a disk).
// live counts bytes sent per request URI while the request is running.
type throttled struct {
	http.ResponseWriter
	rate  int
	start time.Time
	n     int
}

func (t *throttled) Write(b []byte) (int, error) {
	done := 0
	for done < len(b) {
		chunk := len(b) - done
		if chunk > 16<<10 {
			chunk = 16 << 10
		}
		n, err := t.ResponseWriter.Write(b[done : done+chunk])
		done += n
		t.n += n
		if err != nil {
			return done, err
		}
		// No burst credit: time a reader spent not reading (a frozen
		// FFmpeg) does not buy a faster catch-up afterwards.
		time.Sleep(time.Duration(float64(n) / float64(t.rate) * float64(time.Second)))
	}
	return done, nil
}

type counter struct {
	http.ResponseWriter
	n int64
}

func (c *counter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func main() {
	fs := http.FileServer(http.Dir(os.Args[2]))
	var mu sync.Mutex
	log, _ := os.OpenFile(os.Args[3], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	http.ListenAndServe(os.Args[1], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		c := &counter{ResponseWriter: w}
		// /slow/<KB per second>/<file>: the file at that rate.
		var ww http.ResponseWriter = c
		if strings.HasPrefix(r.URL.Path, "/slow/") {
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/slow/"), "/", 2)
			kb, _ := strconv.Atoi(parts[0])
			r.URL.Path = "/" + parts[1]
			ww = &throttled{ResponseWriter: c, rate: kb << 10, start: time.Now()}
		}
		fs.ServeHTTP(ww, r)
		mu.Lock()
		fmt.Fprintf(log, "%s %s %s range=%q sent=%d dur=%.3f\n", start.UTC().Format("15:04:05.000"), r.Method, r.RequestURI, r.Header.Get("Range"), c.n, time.Since(start).Seconds())
		mu.Unlock()
	}))
}
