//go:build !darwin

package storeio

import "os"

func writeDataPages(file *os.File, arena []byte, bufferSize int, pages []Write) error {
	for _, write := range pages {
		if err := writeArenaAt(file, arena, bufferSize, write); err != nil {
			return err
		}
	}
	return nil
}
