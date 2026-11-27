//go:build !unix

package golock

import (
	"os"
	"sync"
)

// Off unix (the Windows desktop build) Go projects do not run; an in-process mutex keeps the API.
var mu sync.Mutex

func tryLock(*os.File) (bool, error) { return mu.TryLock(), nil }

func unlock(*os.File) { mu.Unlock() }
