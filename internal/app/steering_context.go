package app

import (
	"context"
	"fmt"
	"strings"
)

// steeringContext rebuilds a valid provider request after cancelling a response.
// Conversation messages alone omit tool results, so retain bounded durable
// execution evidence and original attachment snapshots before continuing.
func (a *Agent) steeringContext(ctx context.Context, correction string) (string, error) {
	var b strings.Builder
	var references string
	if a.execution != nil && a.execution.db != nil {
		record, err := a.execution.db.GetTask(ctx, a.execution.session.ID)
		if err != nil {
			return "", err
		}
		evidence, err := a.execution.recoveryContext(ctx, record)
		if err != nil {
			return "", err
		}
		b.WriteString(evidence)
		b.WriteByte('\n')
	}
	if a.session != nil {
		artifact, err := a.loadCompaction(ctx)
		if err != nil {
			return "", err
		}
		evidence, err := compactionEvidence(a.session, artifact)
		if err != nil {
			return "", err
		}
		b.WriteString(evidence)
		recent, err := a.compactionRecentEvidence(ctx, artifact)
		if err != nil {
			return "", err
		}
		b.WriteString(recent)
		conversation, err := a.compactionConversation(ctx, a.session)
		if err != nil {
			return "", err
		}
		b.WriteString(conversation)
		b.WriteByte('\n')
		if artifact != nil {
			references, err = a.compactedReferenceHistory(ctx, artifact)
		} else {
			references, err = a.referenceHistory(ctx)
		}
		if err != nil {
			return "", err
		}
	}
	b.WriteString(correction)
	if !a.customProvider {
		measured := b.Len()
		if a.session != nil && a.session.ContextCompaction != nil {
			measured += len(compactedSummaryInstruction(a.session.Summary))
		}
		if a.projectContext != nil {
			snapshot, err := a.projectContext.Refresh()
			if err != nil {
				return "", err
			}
			measured += len(snapshot.Instructions())
		}
		if measured > a.turnContextLimit() {
			return "", fmt.Errorf("rebuilt turn context exceeds %d byte budget", a.turnContextLimit())
		}
	}
	b.WriteString(references)
	return b.String(), nil
}
