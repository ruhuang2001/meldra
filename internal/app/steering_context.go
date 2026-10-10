package app

import (
	"context"
	"strings"
)

// steeringContext rebuilds a valid provider request after cancelling a response.
// Conversation messages alone omit tool results, so retain bounded durable
// execution evidence and original attachment snapshots before continuing.
func (a *Agent) steeringContext(ctx context.Context, correction string) (string, error) {
	var b strings.Builder
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
		b.WriteString(a.session.resumeContext())
		b.WriteByte('\n')
		references, err := a.referenceHistory(ctx)
		if err != nil {
			return "", err
		}
		b.WriteString(references)
	}
	b.WriteString(correction)
	return b.String(), nil
}
