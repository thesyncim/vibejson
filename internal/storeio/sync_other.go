//go:build !darwin && !linux

package storeio

import "os"

func dataSync(file *os.File) error { return file.Sync() }

func dataBarrier(file *os.File) error { return dataSync(file) }
