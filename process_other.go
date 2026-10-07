//go:build !unix

package main

func pidAlive(int) bool {
	return false
}
