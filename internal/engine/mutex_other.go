//go:build !windows

package engine

// acquireMutex is a no-op off Windows (the named-mutex guard is Windows-only); the pidfile staleness
// check in acquireSingleton is the dev guard. Returning handle 0 signals "no mutex held".
func acquireMutex(string) (uintptr, error) { return 0, nil }

func releaseMutex(uintptr) {}
