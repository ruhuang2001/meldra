package app

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"meldra/internal/task"
)

var errCompactionUnresolved = errors.New("context compaction has unresolved evidence")

type compactionRange struct {
	FirstSequence int64 `json:"first_sequence"`
	LastSequence  int64 `json:"last_sequence"`
}
type compactionRequest struct {
	Sequence   int64               `json:"sequence"`
	ControlID  string              `json:"control_id,omitempty"`
	Text       string              `json:"text"`
	References []ReferenceSnapshot `json:"references,omitempty"`
}
type compactionCall struct {
	ID              string             `json:"id"`
	ProviderCallID  string             `json:"provider_call_id"`
	Name            string             `json:"name"`
	Status          task.ToolStatus    `json:"status"`
	ArgumentsDigest string             `json:"arguments_digest"`
	Artifacts       []task.ArtifactRef `json:"artifacts"`
}
type compactionArtifact struct {
	SchemaVersion    int             `json:"schema_version"`
	Focus            string          `json:"focus"`
	Trigger          string          `json:"trigger"`
	FromBytes        int             `json:"from_bytes"`
	ToBytes          int             `json:"to_bytes"`
	KeptMessages     int             `json:"kept_messages"`
	SourceTurns      compactionRange `json:"source_turns"`
	RetainedMessages struct {
		First int `json:"first"`
		Last  int `json:"last"`
	} `json:"retained_messages"`
	Requests       []compactionRequest `json:"requests"`
	CompactedCalls []string            `json:"compacted_calls"`
	Calls          []compactionCall    `json:"calls"`
	Summary        string              `json:"summary"`
	References     []ReferenceSnapshot `json:"references"`
}

func (a *Agent) loadCompaction(ctx context.Context) (*compactionArtifact, error) {
	if a.session == nil || a.session.ContextCompaction == nil {
		if a.session != nil && a.session.CompactedThroughSequence != 0 {
			return nil, fmt.Errorf("missing context compaction artifact")
		}
		return nil, nil
	}
	if a.execution == nil || a.execution.db == nil {
		return nil, fmt.Errorf("compaction history requires task storage")
	}
	ref := *a.session.ContextCompaction
	if ref.Name != "context-compaction.json" {
		return nil, fmt.Errorf("invalid context compaction artifact name")
	}
	data, err := a.execution.db.ReadArtifact(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("read context compaction: %w", err)
	}
	var result compactionArtifact
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid UTF-8 context compaction")
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode context compaction: %w", err)
	}
	if result.SchemaVersion != 1 || strings.TrimSpace(result.Summary) == "" || !utf8.ValidString(result.Summary) || len(result.Summary) > maxSessionSummaryBytes || result.SourceTurns.LastSequence != a.session.CompactedThroughSequence || result.SourceTurns.FirstSequence < 0 || result.SourceTurns.LastSequence < result.SourceTurns.FirstSequence {
		return nil, fmt.Errorf("invalid context compaction metadata")
	}
	seenRequests := map[int64]bool{}
	seenControls := map[string]bool{}
	refs := slices.Clone(result.References)
	for _, request := range result.Requests {
		if request.Sequence == 0 || seenRequests[request.Sequence] || request.Sequence > result.SourceTurns.LastSequence || request.Text == "" || !utf8.ValidString(request.Text) || len(request.ControlID) > 256 || request.ControlID != "" && seenControls[request.ControlID] {
			return nil, fmt.Errorf("invalid context compaction request")
		}
		seenRequests[request.Sequence] = true
		if request.ControlID != "" {
			seenControls[request.ControlID] = true
		}
		refs = append(refs, request.References...)
	}
	seenCalls := map[string]bool{}
	if len(result.CompactedCalls) != len(result.Calls) {
		return nil, fmt.Errorf("invalid context compaction call index")
	}
	for i, call := range result.Calls {
		_, digestErr := hex.DecodeString(call.ArgumentsDigest)
		if call.ID == "" || seenCalls[call.ID] || result.CompactedCalls[i] != call.ID || call.Name == "" || len(call.ArgumentsDigest) != 64 || digestErr != nil || !compactionCallResolved(call.Status) {
			return nil, fmt.Errorf("invalid context compaction call")
		}
		seenCalls[call.ID] = true
		for _, ref := range call.Artifacts {
			if _, err := a.execution.db.ReadArtifact(ctx, ref); err != nil {
				return nil, fmt.Errorf("read compacted tool artifact: %w", err)
			}
		}
	}
	if err := a.verifyCompactionReferences(ctx, refs); err != nil {
		return nil, err
	}
	return &result, nil
}

func (a *Agent) verifyCompactionReferences(ctx context.Context, refs []ReferenceSnapshot) error {
	seen := map[task.ArtifactRef]bool{}
	for _, ref := range refs {
		if err := validateReferences([]ReferenceSnapshot{ref}); err != nil {
			return fmt.Errorf("invalid compacted reference: %w", err)
		}
		if seen[ref.Artifact] {
			continue
		}
		if _, err := a.execution.db.ReadArtifact(ctx, ref.Artifact); err != nil {
			return fmt.Errorf("read compacted reference: %w", err)
		}
		seen[ref.Artifact] = true
	}
	return nil
}

func compactionCallResolved(status task.ToolStatus) bool {
	return status == task.ToolSucceeded || status == task.ToolFailed || status == task.ToolDeclined || status == task.ToolCancelled
}

type compactionHistoryRequest struct {
	compactionRequest
	runID        string
	turnSequence int64
}

func (a *Agent) compactionHistory(ctx context.Context, keep int) (*compactionArtifact, error) {
	if a.session == nil || a.execution == nil || a.execution.db == nil {
		return nil, fmt.Errorf("compaction history requires task storage")
	}
	if keep < 0 || keep > len(a.session.Messages) {
		return nil, fmt.Errorf("invalid compaction retained message count")
	}
	db, id := a.execution.db, a.session.ID
	calls, err := db.ToolCalls(ctx, id)
	if err != nil {
		return nil, err
	}
	approvals, err := db.Approvals(ctx, id)
	if err != nil {
		return nil, err
	}
	processes, err := db.Processes(ctx, id)
	if err != nil {
		return nil, err
	}
	unresolved := 0
	for _, call := range calls {
		if !compactionCallResolved(call.Status) {
			unresolved++
		}
	}
	for _, approval := range approvals {
		if approval.Decision == task.Pending {
			unresolved++
		}
	}
	for _, process := range processes {
		if process.Effects == "unknown" || process.State == task.ProcessUnknown && process.Effects != "resolved" {
			unresolved++
		}
	}
	if unresolved > 0 {
		return nil, fmt.Errorf("%w: %d record(s). Inspect: meldra task show %s", errCompactionUnresolved, unresolved, id)
	}
	previous, err := a.loadCompaction(ctx)
	if err != nil {
		return nil, err
	}
	result := &compactionArtifact{SchemaVersion: 1, KeptMessages: keep}
	if previous != nil {
		result.Requests = slices.Clone(previous.Requests)
		result.Calls = slices.Clone(previous.Calls)
		result.CompactedCalls = slices.Clone(previous.CompactedCalls)
		result.References = slices.Clone(previous.References)
		result.SourceTurns = previous.SourceTurns
	}
	result.RetainedMessages.First = len(a.session.Messages) - keep + 1
	result.RetainedMessages.Last = len(a.session.Messages)
	var events []task.Event
	var after int64
	for {
		page, err := db.Events(ctx, id, after, 1000)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			if event.Sequence <= after {
				return nil, fmt.Errorf("invalid task event sequence")
			}
			after = event.Sequence
			events = append(events, event)
		}
	}
	var requests []compactionHistoryRequest
	runSequences := map[string]int64{}
	queuedControls := map[string]bool{}
	requestRefs := map[int64][]ReferenceSnapshot{}
	for _, event := range events {
		if event.Kind == "turn.started" {
			input, err := decodeRecordedRequest(ctx, db, event)
			if err != nil {
				return nil, err
			}
			requests = append(requests, compactionHistoryRequest{compactionRequest: compactionRequest{Sequence: event.Sequence, ControlID: input.ControlID, Text: input.Request, References: input.References}, runID: event.RunID, turnSequence: event.Sequence})
			runSequences[event.RunID] = event.Sequence
			if input.ControlID != "" {
				queuedControls[input.ControlID] = true
			}
		}
		if event.Kind == "request.references" {
			var record struct {
				RequestSequence int64               `json:"request_sequence"`
				References      []ReferenceSnapshot `json:"references"`
			}
			if err := json.Unmarshal(event.Data, &record); err != nil {
				return nil, err
			}
			if err := validateReferences(record.References); err != nil {
				return nil, err
			}
			requestRefs[record.RequestSequence] = record.References
		}
	}
	for i := range requests {
		if refs, ok := requestRefs[requests[i].Sequence]; ok {
			requests[i].References = refs
		}
	}
	savedControls := map[string]SessionMessage{}
	appliedControls := map[string]bool{}
	for _, message := range a.session.Messages {
		if message.ControlID != "" {
			savedControls[message.ControlID] = message
		}
	}
	for _, event := range events {
		if event.Kind == "control.applied" {
			var control ControlRequest
			if err := json.Unmarshal(event.Data, &control); err != nil {
				return nil, err
			}
			appliedControls[control.ID] = true
		}
	}
	for _, event := range events {
		if event.Kind != "control.applied" && event.Kind != "control.received" {
			continue
		}
		var control ControlRequest
		if err := json.Unmarshal(event.Data, &control); err != nil {
			return nil, err
		}
		if control.Kind != "steer" || queuedControls[control.ID] {
			continue
		}
		if event.Kind == "control.received" {
			message, saved := savedControls[control.ID]
			if appliedControls[control.ID] || !saved {
				continue
			}
			if message.Role != "user" || message.Content != control.Text {
				return nil, fmt.Errorf("saved steering request does not match received control")
			}
		}
		if control.ID == "" || control.Text == "" || len(control.Text) > maxSessionMessageBytes || !utf8.ValidString(control.Text) {
			return nil, fmt.Errorf("invalid recorded steering request")
		}
		if err := validateReferences(control.References); err != nil {
			return nil, err
		}
		sequence := runSequences[event.RunID]
		if sequence == 0 {
			return nil, fmt.Errorf("steering request has no recorded turn")
		}
		requests = append(requests, compactionHistoryRequest{compactionRequest: compactionRequest{Sequence: event.Sequence, ControlID: control.ID, Text: control.Text, References: control.References}, runID: event.RunID, turnSequence: sequence})
	}
	slices.SortFunc(requests, func(x, y compactionHistoryRequest) int {
		return cmp.Compare(x.Sequence, y.Sequence)
	})
	bySequence := map[int64]int{}
	for i, request := range requests {
		bySequence[request.Sequence] = i
	}
	// Older snapshots lack sequence IDs. Backward alignment uses content and
	// control identity, never timestamps, preserving distinct repeated requests.
	mapped := make([]int64, len(a.session.Messages))
	cursor := len(requests) - 1
	// An automatic compaction starts after the incoming request event exists,
	// but before that request is appended to the snapshot being compacted.
	for cursor >= 0 && a.execution.requestSequence != 0 && requests[cursor].Sequence >= a.execution.requestSequence {
		cursor--
	}
	for i := len(a.session.Messages) - 1; i >= 0; i-- {
		message := a.session.Messages[i]
		if message.Role != "user" {
			continue
		}
		if message.RequestSequence > 0 {
			index, ok := bySequence[message.RequestSequence]
			if !ok || requests[index].ControlID != message.ControlID || truncateSessionMessage(requests[index].Text) != message.Content {
				return nil, fmt.Errorf("snapshot request does not match recorded history")
			}
			mapped[i] = message.RequestSequence
			cursor = index - 1
			continue
		}
		for j := cursor; j >= 0; j-- {
			if requests[j].ControlID == message.ControlID && truncateSessionMessage(requests[j].Text) == message.Content {
				mapped[i] = requests[j].Sequence
				cursor = j - 1
				break
			}
		}
	}
	split := len(a.session.Messages) - keep
	boundary := after + 1
	if keep > 0 {
		start := split
		for start > 0 && a.session.Messages[start].Role != "user" {
			start--
		}
		if a.session.Messages[start].Role != "user" && len(requests) > 0 {
			// A previous exact-tail compaction may have moved this assistant's
			// user request into its artifact. Recover its turn from the ordered
			// requests immediately before the next retained user request.
			before := after + 1
			if a.execution.requestSequence != 0 {
				before = a.execution.requestSequence
			}
			for i := start; i < len(mapped); i++ {
				if mapped[i] > 0 {
					before = mapped[i]
					break
				}
			}
			found := false
			for i := len(requests) - 1; i >= 0; i-- {
				if requests[i].Sequence < before {
					boundary = requests[i].turnSequence
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("cannot identify retained assistant turn")
			}
		}
		for i := start; i < len(mapped); i++ {
			if a.session.Messages[i].Role != "user" {
				continue
			}
			if mapped[i] == 0 {
				if len(requests) > 0 {
					return nil, fmt.Errorf("cannot identify retained request boundary")
				}
				continue
			}
			boundary = min(boundary, requests[bySequence[mapped[i]]].turnSequence)
		}
	}
	seenRequests, seenControls, seenCalls, seenRefs := map[int64]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	negative := int64(-1)
	for _, request := range result.Requests {
		seenRequests[request.Sequence] = true
		if request.ControlID != "" {
			seenControls[request.ControlID] = true
		}
		negative = min(negative, request.Sequence-1)
	}
	for _, call := range result.Calls {
		seenCalls[call.ID] = true
	}
	for _, ref := range result.References {
		seenRefs[ref.Digest] = true
	}
	removedRequests := map[int64]bool{}
	for i, message := range a.session.Messages[:split] {
		if message.Role == "user" && mapped[i] != 0 {
			removedRequests[mapped[i]] = true
		}
	}
	addRequest := func(request compactionRequest) {
		if seenRequests[request.Sequence] || request.ControlID != "" && seenControls[request.ControlID] {
			return
		}
		seenRequests[request.Sequence] = true
		if request.ControlID != "" {
			seenControls[request.ControlID] = true
		}
		result.Requests = append(result.Requests, request)
		for _, ref := range request.References {
			if !seenRefs[ref.Digest] {
				result.References = append(result.References, ref)
				seenRefs[ref.Digest] = true
			}
		}
	}
	foldedRuns := map[string]bool{}
	for _, request := range requests {
		fullyFolded := request.turnSequence < boundary
		if !fullyFolded && !removedRequests[request.Sequence] {
			continue
		}
		addRequest(request.compactionRequest)
		// A tail beginning with an assistant keeps that turn's call evidence
		// outside the fold, while its removed user request remains verbatim.
		if fullyFolded {
			foldedRuns[request.runID] = true
		}
		if result.SourceTurns.FirstSequence == 0 {
			result.SourceTurns.FirstSequence = request.Sequence
		}
		result.SourceTurns.FirstSequence = min(result.SourceTurns.FirstSequence, request.Sequence)
		result.SourceTurns.LastSequence = max(result.SourceTurns.LastSequence, request.Sequence)
	}
	for i, message := range a.session.Messages[:split] {
		if message.Role != "user" {
			continue
		}
		if mapped[i] == 0 {
			addRequest(compactionRequest{Sequence: negative, ControlID: message.ControlID, Text: message.Content, References: message.References})
			negative--
			continue
		}
		for j := range result.Requests {
			if result.Requests[j].Sequence == mapped[i] {
				for _, ref := range message.References {
					if !seenRefs[ref.Digest] {
						result.References = append(result.References, ref)
						seenRefs[ref.Digest] = true
					}
				}
			}
		}
	}
	for _, call := range calls {
		if !foldedRuns[call.RunID] || seenCalls[call.ID] {
			continue
		}
		for _, ref := range call.Result.Artifacts {
			if _, err := db.ReadArtifact(ctx, ref); err != nil {
				return nil, fmt.Errorf("read folded tool artifact: %w", err)
			}
		}
		result.Calls = append(result.Calls, compactionCall{ID: call.ID, ProviderCallID: call.ProviderCallID, Name: call.Name, Status: call.Status, ArgumentsDigest: digest(call.Arguments), Artifacts: slices.Clone(call.Result.Artifacts)})
		result.CompactedCalls = append(result.CompactedCalls, call.ID)
		seenCalls[call.ID] = true
	}
	if err := a.verifyCompactionReferences(ctx, result.References); err != nil {
		return nil, err
	}
	return result, nil
}
