//go:build !linux

package storeio

import "os"

func dataSync(file *os.File) error { return file.Sync() }
