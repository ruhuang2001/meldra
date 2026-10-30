package app

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

// recordedUserRequest keeps event previews bounded while preserving the full
// original request in a digest-checked artifact when the snapshot would truncate it.
type recordedUserRequest struct {
	Request         string              `json:"request"`
	ControlID       string              `json:"control_id"`
	References      []ReferenceSnapshot `json:"references"`
	RequestArtifact *task.ArtifactRef   `json:"request_artifact,omitempty"`
}

func decodeRecordedRequest(ctx context.Context, db *taskstore.Store, event task.Event) (recordedUserRequest, error) {
	var input recordedUserRequest
	if event.Kind != "turn.started" || !utf8.Valid(event.Data) {
		return input, fmt.Errorf("invalid recorded request event")
	}
	if err := json.Unmarshal(event.Data, &input); err != nil {
		return input, fmt.Errorf("decode recorded request: %w", err)
	}
	if input.Request == "" || !utf8.ValidString(input.Request) || len(input.Request) > maxSessionMessageBytes || len(input.ControlID) > 256 {
		return input, fmt.Errorf("invalid recorded request preview")
	}
	if err := validateReferences(input.References); err != nil {
		return input, fmt.Errorf("invalid recorded request references: %w", err)
	}
	if input.RequestArtifact == nil {
		return input, nil
	}
	ref := *input.RequestArtifact
	if db == nil || ref.Name != "user-request.txt" || ref.Size <= maxSessionMessageBytes {
		return input, fmt.Errorf("invalid recorded request artifact")
	}
	data, err := db.ReadArtifact(ctx, ref)
	if err != nil {
		return input, fmt.Errorf("read recorded request artifact: %w", err)
	}
	if !utf8.Valid(data) || truncateSessionMessage(string(data)) != input.Request {
		return input, fmt.Errorf("recorded request artifact does not match its preview")
	}
	input.Request = string(data)
	return input, nil
}
