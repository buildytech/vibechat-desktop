//go:build !linux

package main

// ownLikeData: the server runs on Linux only; elsewhere (development) files stay with the current user.
func ownLikeData(string, string) error { return nil }
