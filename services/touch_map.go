package services

import (
	"os"
	"time"

	"github.com/webtor-io/lazymap"
)

// TouchMap tracks last access time per output directory by maintaining a
// {hash}.touch file alongside the hash directory. External cleanup processes
// use the .touch mtime to determine whether content is still being accessed.
type TouchMap struct {
	*lazymap.LazyMap[bool]
}

func NewTouchMap() *TouchMap {
	return &TouchMap{
		LazyMap: lazymap.New[bool](&lazymap.Config{
			Expire: 30 * time.Second,
		}),
	}
}

func (s *TouchMap) touch(path string) (bool, error) {
	f := path + ".touch"
	_, err := os.Stat(f)
	if os.IsNotExist(err) {
		file, err := os.Create(f)
		if err != nil {
			return false, err
		}
		defer file.Close()
	} else {
		currentTime := time.Now().Local()
		err = os.Chtimes(f, currentTime, currentTime)
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *TouchMap) Touch(path string) (bool, error) {
	return s.LazyMap.Get(path, func() (bool, error) {
		return s.touch(path)
	})
}
