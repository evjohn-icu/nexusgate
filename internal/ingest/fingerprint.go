package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const sampleSize int64 = 4 << 20

func QuickFingerprint(path string, size int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := fmt.Fprintf(hash, "%d:", size); err != nil {
		return "", err
	}

	offsets := uniqueOffsets(size)
	buffer := make([]byte, sampleSize)
	for _, offset := range offsets {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
		limit := sampleSize
		if remaining := size - offset; remaining < limit {
			limit = remaining
		}
		n, err := io.ReadFull(file, buffer[:limit])
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return "", err
		}
		if _, err := hash.Write(buffer[:n]); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func uniqueOffsets(size int64) []int64 {
	candidates := []int64{0}
	if size > sampleSize {
		middle := size/2 - sampleSize/2
		if middle > 0 {
			candidates = append(candidates, middle)
		}
		end := size - sampleSize
		if end > 0 {
			candidates = append(candidates, end)
		}
	}

	seen := make(map[int64]struct{}, len(candidates))
	offsets := make([]int64, 0, len(candidates))
	for _, offset := range candidates {
		if _, ok := seen[offset]; ok {
			continue
		}
		seen[offset] = struct{}{}
		offsets = append(offsets, offset)
	}
	return offsets
}
