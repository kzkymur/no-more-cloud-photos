package readapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const CursorKindChangesV1 = byte(0x03)
const DefaultChangesLimit = 100
const MaxChangesLimit = 1000

type ChangesRequest struct {
	Cursor string
	Limit  int
}
type ChangeEvent struct {
	ID         string          `json:"id"`
	Position   string          `json:"position"`
	Type       string          `json:"type"`
	MediaID    string          `json:"media_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Media      json.RawMessage `json:"media"`
	Reason     string          `json:"reason"`
}
type ChangesPage struct {
	Items      []ChangeEvent `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

func changesFingerprint() [sha256.Size]byte {
	return sha256.Sum256(append([]byte(CursorFilterDomainV1), CursorVersionV1, CursorKindChangesV1))
}

// EncodeChanges uses the common authenticated envelope with a signed int64
// position in the tuple slot; the UUID slot is reserved and must be all zero.
func (codec *CursorCodec) EncodeChanges(position int64) (string, error) {
	if codec == nil || len(codec.key) < sha256.Size || position < 0 {
		return "", ErrInvalidCursor
	}
	return codec.encode(CursorKindChangesV1, cursorClassValue, changesFingerprint(), position, [cursorUUIDSize]byte{}), nil
}
func (codec *CursorCodec) DecodeChanges(token string) (int64, error) {
	payload, ok := codec.authenticate(token)
	fingerprint := changesFingerprint()
	if !ok || !validHeader(payload, CursorKindChangesV1) || payload[len(CursorMagicV1)+2] != cursorClassValue || !hmac.Equal(payload[cursorHeaderSize:cursorHeaderSize+sha256.Size], fingerprint[:]) {
		return 0, ErrInvalidCursor
	}
	for _, b := range payload[cursorPayloadSize-cursorUUIDSize : cursorPayloadSize] {
		if b != 0 {
			return 0, ErrInvalidCursor
		}
	}
	position := int64(binary.BigEndian.Uint64(payload[cursorHeaderSize+sha256.Size : cursorHeaderSize+sha256.Size+8]))
	if position < 0 {
		return 0, ErrInvalidCursor
	}
	return position, nil
}

// ListChanges reads only immutable snapshots, never joins live media. Empty
// pages return a resumable zero/last position rather than closing the feed.
func (s *Service) ListChanges(ctx context.Context, request ChangesRequest) (ChangesPage, error) {
	if request.Limit < 1 || request.Limit > MaxChangesLimit {
		return ChangesPage{}, NewInvalidRequest(map[string]string{"limit": "out_of_range"})
	}
	var position int64
	if request.Cursor != "" {
		var err error
		position, err = s.cursors.DecodeChanges(request.Cursor)
		if err != nil {
			return ChangesPage{}, NewInvalidCursor()
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,position,event_type,media_id::text,occurred_at,payload,reason FROM change_events WHERE position>$1 ORDER BY position LIMIT $2`, position, request.Limit)
	if err != nil {
		return ChangesPage{}, classifyDatabaseError(err)
	}
	defer rows.Close()
	page := ChangesPage{Items: make([]ChangeEvent, 0)}
	for rows.Next() {
		var event ChangeEvent
		var next int64
		var payload []byte
		if err := rows.Scan(&event.ID, &next, &event.Type, &event.MediaID, &event.OccurredAt, &payload, &event.Reason); err != nil {
			return ChangesPage{}, classifyDatabaseError(err)
		}
		if next <= position {
			return ChangesPage{}, NewInvariantError(errors.New("invalid feed ordering"))
		}
		event.Position = strconv.FormatInt(next, 10)
		event.OccurredAt = event.OccurredAt.UTC()
		event.Media = json.RawMessage(payload)
		if payload == nil {
			event.Media = json.RawMessage("null")
		}
		page.Items = append(page.Items, event)
		position = next
	}
	if err := rows.Err(); err != nil {
		return ChangesPage{}, classifyDatabaseError(err)
	}
	page.NextCursor, err = s.cursors.EncodeChanges(position)
	if err != nil {
		return ChangesPage{}, NewInvariantError(err)
	}
	return page, nil
}
