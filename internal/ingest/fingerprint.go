package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
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

// StableAssetKey derives the identity the pipeline keys its probe job's input
// hash on: fingerprint + size + mtime, and nothing else. The path is
// deliberately absent: asset identity is fingerprint+size (see
// UpsertScannedFile), so a rename, remount or case change of a file resolves
// to the SAME asset row — the probe hash must therefore not change with the
// path, or every moved file would re-enqueue the whole downstream chain and
// pay for a fresh analysis of identical content. A plain copy (new inode,
// fresh mtime) does change the key and re-enqueues the chain: the price of
// keeping mtime in the identity, which is the cheapest faithful signal that
// the content actually changed — a real edit bumps the mtime even when it
// lands outside the sampled fingerprint windows, and only then should the
// chain re-run. Two files sharing sampled bytes, size and mtime collapse
// onto one key — acceptable, because they are already the same asset row.
func StableAssetKey(fingerprint string, size int64, modifiedNS int64) string {
	h := sha256.New()
	h.Write([]byte(fingerprint))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(size, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(modifiedNS, 10)))
	return hex.EncodeToString(h.Sum(nil))
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
