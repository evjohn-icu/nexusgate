package sqlite

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"
)

func floatBlob(values ...float32) []byte {
	out := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out
}

func TestVecCosineFunctionIsRegistered(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var distance float64
	if err := repo.db.QueryRowContext(context.Background(), `SELECT vec_distance_cosine(?,?)`,
		floatBlob(1, 0), floatBlob(1, 0)).Scan(&distance); err != nil {
		t.Fatalf("vec_distance_cosine: %v", err)
	}
	if math.Abs(distance) > 1e-6 {
		t.Fatalf("identical vectors distance=%v", distance)
	}
	if err := repo.db.QueryRowContext(context.Background(), `SELECT vec_distance_cosine(?,?)`,
		floatBlob(1, 0), floatBlob(0, 1)).Scan(&distance); err != nil {
		t.Fatal(err)
	}
	if math.Abs(distance-1) > 1e-6 {
		t.Fatalf("orthogonal vectors distance=%v", distance)
	}
}
