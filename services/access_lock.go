package services

import "sync"

type AccessLock struct {
	C      chan error
	closed bool
	mux    sync.Mutex
}

func NewAccessLock() *AccessLock {
	return &AccessLock{C: make(chan error)}
}
func (al *AccessLock) Unlocked() chan error {
	return al.C
}
func (al *AccessLock) Unlock() {
	al.mux.Lock()
	defer al.mux.Unlock()
	if !al.closed {
		close(al.C)
		al.closed = true
	}
}
