package cachecoord

import (
	"context"
	"testing"
	"time"
)

func TestSharedLockBlocksExclusiveUntilRelease(t *testing.T) {
	dir := t.TempDir()
	reader, err := AcquireShared(dir)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	acquired := make(chan *Lock)
	go func() {
		close(started)
		lock, err := AcquireExclusive(dir)
		if err == nil {
			acquired <- lock
		}
	}()
	<-started
	select {
	case lock := <-acquired:
		_ = lock.Release()
		t.Fatal("exclusive lock acquired while reader was held")
	default:
	}
	if err := reader.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case lock := <-acquired:
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("exclusive lock did not acquire after reader release")
	}
}
