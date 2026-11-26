package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/capture"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

// RebuildAutomaticShootSessions materializes deterministic, conservative
// capture groups for one library root. Manual sessions are intentionally left
// untouched; source files and metadata are never modified.
//
// Rebuilding does not rotate session IDs blindly. Within the rebuild
// transaction, old and new automatic groups are matched by shared asset IDs
// (largest overlap first, ties broken by old session id and then the new
// group's sorted member-id sequence) and every matched session keeps its
// persisted id, with metadata and asset links rewritten in place. Saved
// collection filters and deep links reference session ids, so an unchanged
// pipeline pass must reuse them; only truly obsolete automatic sessions are
// removed, last, after every surviving group holds its reused or new id.
func (r *Repository) RebuildAutomaticShootSessions(ctx context.Context, rootID string) error {
	tx, err := r.beginImmediateWrite(ctx)
	if err != nil {
		return err
	}
	defer rollbackAndClose(tx)
	rows, err := tx.QueryContext(ctx, `SELECT a.id,COALESCE(al.relative_path,''),COALESCE(cm.vendor,''),COALESCE(cm.model,''),COALESCE(cm.device_serial,''),cm.captured_at,COALESCE(m.duration_ms,0),COALESCE(cm.session_marker,''),COALESCE(cm.reel,'') FROM assets a JOIN asset_locations al ON al.id=(SELECT l.id FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=a.id AND l.root_id=? AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.is_primary DESC,l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1) LEFT JOIN capture_metadata cm ON cm.asset_id=a.id LEFT JOIN media_metadata m ON m.asset_id=a.id WHERE cm.captured_at IS NOT NULL GROUP BY a.id`, rootID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var captures []capture.Capture
	for rows.Next() {
		var item capture.Capture
		var vendor, model, serial, captured, sessionMarker, reel string
		var durationMS int64
		if err := rows.Scan(&item.ID, &item.Context.Filename, &vendor, &model, &serial, &captured, &durationMS, &sessionMarker, &reel); err != nil {
			return err
		}
		item.Source = capture.SourceOriginal
		item.Identity = capture.CaptureIdentity{DeviceSerial: serial, Manufacturer: vendor, Model: model}
		// Only a real device identity may join captures automatically: an
		// invented "vendor:model" key for files with neither vendor nor model
		// would merge unrelated footage through a value no camera reported.
		if strings.TrimSpace(vendor) != "" || strings.TrimSpace(model) != "" {
			item.Identity.DeviceID = strings.TrimSpace(vendor + ":" + model)
		}
		item.Context.CapturedAt, _ = time.Parse(time.RFC3339Nano, captured)
		item.Context.Duration = time.Duration(durationMS) * time.Millisecond
		item.Context.SessionMarker = sessionMarker
		item.Context.ReelMarker = reel
		captures = append(captures, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sessions := capture.AggregateSessions(captures)
	var oldIDs []string
	oldGroupsByAsset := make(map[string][]string)
	oldRows, err := tx.QueryContext(ctx, `SELECT ss.id,ass.asset_id FROM shoot_sessions ss LEFT JOIN asset_shoot_sessions ass ON ass.session_id=ss.id WHERE ss.root_id=? AND ss.state='automatic' ORDER BY ss.id,ass.asset_id`, rootID)
	if err != nil {
		return err
	}
	for oldRows.Next() {
		var sessionID string
		var assetID sql.NullString
		if err := oldRows.Scan(&sessionID, &assetID); err != nil {
			oldRows.Close()
			return err
		}
		if len(oldIDs) == 0 || oldIDs[len(oldIDs)-1] != sessionID {
			oldIDs = append(oldIDs, sessionID)
		}
		if assetID.Valid {
			oldGroupsByAsset[assetID.String] = append(oldGroupsByAsset[assetID.String], sessionID)
		}
	}
	if err := oldRows.Err(); err != nil {
		oldRows.Close()
		return err
	}
	oldRows.Close()
	matched := matchSessionGroups(oldGroupsByAsset, sessions)
	now := formatTime(time.Now())
	for index, session := range sessions {
		id, reuse := matched[index]
		if !reuse {
			id = idgen.New()
		}
		cameraLabel := strings.TrimSpace(session.Identity.Manufacturer + " " + session.Identity.Model)
		title := cameraLabel
		if !session.Start.IsZero() {
			title = session.Start.Local().Format("2006-01-02 15:04") + " · " + cameraLabel
		}
		if reuse {
			if _, err := tx.ExecContext(ctx, `UPDATE shoot_sessions SET title=?,starts_at=?,ends_at=?,camera_label=?,confidence=?,updated_at=? WHERE id=?`, title, nullableTime(&session.Start), nullableTime(&session.End), cameraLabel, 0.8, now, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM asset_shoot_sessions WHERE session_id=?`, id); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO shoot_sessions(id,root_id,title,state,starts_at,ends_at,camera_label,confidence,created_at,updated_at) VALUES(?,?,?,'automatic',?,?,?,?,?,?)`, id, rootID, title, nullableTime(&session.Start), nullableTime(&session.End), cameraLabel, 0.8, now, now); err != nil {
				return err
			}
		}
		for _, item := range session.Captures {
			if _, err := tx.ExecContext(ctx, `INSERT INTO asset_shoot_sessions(asset_id,session_id,is_primary,created_at) VALUES(?,?,1,?)`, item.ID, id, now); err != nil {
				return err
			}
		}
	}
	// Remove truly obsolete automatic sessions last: their asset links cascade,
	// and they are only leftovers once every surviving group has been written.
	kept := make(map[string]struct{}, len(matched))
	for _, oldID := range matched {
		kept[oldID] = struct{}{}
	}
	for _, oldID := range oldIDs {
		if _, ok := kept[oldID]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM shoot_sessions WHERE id=? AND root_id=? AND state='automatic'`, oldID, rootID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `COMMIT`)
	return err
}

// matchSessionGroups pairs new automatic groups with persisted ones by shared
// asset ids so surviving sessions keep their ids. Each unmatched old/new pair
// scores one point per shared asset; pairs are consumed greedily, largest
// overlap first, ties broken by old session id and then by the new group's
// lexicographically sorted member-id sequence, and neither side is matched
// twice. Groups with no shared assets never match: the old session is
// obsolete and the new group gets a fresh id.
func matchSessionGroups(oldGroupsByAsset map[string][]string, sessions []capture.ShootSession) map[int]string {
	type pair struct {
		oldID      string
		newIndex   int
		overlap    int
		memberRank string
	}
	var pairs []pair
	for index, session := range sessions {
		overlaps := make(map[string]int)
		for _, item := range session.Captures {
			for _, oldID := range oldGroupsByAsset[item.ID] {
				overlaps[oldID]++
			}
		}
		if len(overlaps) == 0 {
			continue
		}
		ids := make([]string, 0, len(session.Captures))
		for _, item := range session.Captures {
			ids = append(ids, item.ID)
		}
		sort.Strings(ids)
		memberRank := strings.Join(ids, "\x00")
		for oldID, overlap := range overlaps {
			pairs = append(pairs, pair{oldID: oldID, newIndex: index, overlap: overlap, memberRank: memberRank})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].overlap != pairs[j].overlap {
			return pairs[i].overlap > pairs[j].overlap
		}
		if pairs[i].oldID != pairs[j].oldID {
			return pairs[i].oldID < pairs[j].oldID
		}
		return pairs[i].memberRank < pairs[j].memberRank
	})
	matched := make(map[int]string)
	usedOld := make(map[string]struct{})
	for _, candidate := range pairs {
		if _, taken := matched[candidate.newIndex]; taken {
			continue
		}
		if _, taken := usedOld[candidate.oldID]; taken {
			continue
		}
		matched[candidate.newIndex] = candidate.oldID
		usedOld[candidate.oldID] = struct{}{}
	}
	return matched
}
