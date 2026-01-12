package services

import (
	"os"
	"time"

	"github.com/webtor-io/lazymap"
)

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
		defer func(file *os.File) {
			_ = file.Close()
		}(file)
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
