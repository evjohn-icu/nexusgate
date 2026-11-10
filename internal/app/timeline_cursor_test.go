package app

import (
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestPaginateTimelineResumesWithoutSkipping(t *testing.T) {
	rows := []domain.AssetShot{
		{ID: "shot-c", StartMS: 2000, EndMS: 3000},
		{ID: "shot-a", StartMS: 0, EndMS: 1000},
		{ID: "shot-b", StartMS: 1000, EndMS: 2000},
	}
	first, cursor, truncated := paginateTimeline(rows, 2, "",
		func(a, b domain.AssetShot) bool { return shotLess(a, b) },
		func(row domain.AssetShot) string { return timelineCursor(row.StartMS, row.EndMS, row.ID) })
	if !truncated || cursor == "" || len(first) != 2 || first[0].ID != "shot-a" || first[1].ID != "shot-b" {
		t.Fatalf("first page=%+v cursor=%q truncated=%v", first, cursor, truncated)
	}
	second, next, more := paginateTimeline(rows, 2, cursor,
		func(a, b domain.AssetShot) bool { return shotLess(a, b) },
		func(row domain.AssetShot) string { return timelineCursor(row.StartMS, row.EndMS, row.ID) })
	if more || next != "" || len(second) != 1 || second[0].ID != "shot-c" {
		t.Fatalf("second page=%+v cursor=%q more=%v", second, next, more)
	}
}
