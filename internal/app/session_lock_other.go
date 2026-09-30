//go:build !darwin && !linux

package app

import "sync"

var sessionStoreSaveMutex sync.Mutex

func acquireSessionStoreLock(string) (func(), error) {
	sessionStoreSaveMutex.Lock()
	return sessionStoreSaveMutex.Unlock, nil
}
