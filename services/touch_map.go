package services

import (
	"os"
	"time"

	"github.com/webtor-io/lazymap"
)

type TouchMap struct {
	lazymap.LazyMap
}

func NewTouchMap() *TouchMap {
	return &TouchMap{
		LazyMap: lazymap.New(&lazymap.Config{
			Expire: 30 * time.Second,
		}),
	}
}

func (s *TouchMap) touch(path string) error {
	f := path + ".touch"
	_, err := os.Stat(f)
	if os.IsNotExist(err) {
		file, err := os.Create(f)
		if err != nil {
			return err
		}
		defer func(file *os.File) {
			_ = file.Close()
		}(file)
	} else {
		currentTime := time.Now().Local()
		err = os.Chtimes(f, currentTime, currentTime)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *TouchMap) Touch(path string) error {
	_, err := s.LazyMap.Get(path, func() (interface{}, error) {
		err := s.touch(path)
		return nil, err
	})
	return err
}
