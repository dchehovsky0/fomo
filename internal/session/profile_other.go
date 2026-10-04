//go:build !windows

package session

func stopHeadlessChrome(string) (bool, error) { return false, nil }
