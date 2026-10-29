package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"meldra/internal/task"
)

// Stream approval evidence binds the full preview (including truncated tails),
// policy, inspected project scopes and the tool's own file/command fingerprints.
type approvalDetail struct {
	Title              string            `json:"title"`
	Detail             string            `json:"detail"`
	Source             string            `json:"source"`
	Mode               ExecutionMode     `json:"mode"`
	Permissions        PermissionProfile `json:"permissions"`
	Generation         uint64            `json:"generation"`
	FullDetailHash     string            `json:"full_detail_hash,omitempty"`
	ApprovedDetailHash string            `json:"approved_detail_hash,omitempty"`
	ContextDigest      string            `json:"context_digest,omitempty"`
	ContextPaths       []string          `json:"context_paths,omitempty"`
	UndoEvidence       *task.ArtifactRef `json:"undo_evidence,omitempty"`
	MCPServer          string            `json:"mcp_server,omitempty"`
	MCPBinding         string            `json:"mcp_binding,omitempty"`
}

func approvalHash(approval task.Approval) string {
	raw, _ := json.Marshal(approval)
	return digest(raw)
}

func (e *taskExecution) loadApproval(ctx context.Context) error {
	if e.approveDetailHash == "" {
		return nil
	}
	approvals, err := e.db.PendingApprovals(ctx, e.session.ID)
	if err != nil {
		return err
	}
	for _, approval := range approvals {
		if approval.Decision != task.Pending || approvalHash(approval) != e.approveDetailHash {
			continue
		}
		call, err := e.db.GetToolCall(ctx, approval.ToolCallID)
		if err != nil {
			return err
		}
		run, err := e.db.GetRun(ctx, approval.RunID)
		if err != nil {
			return err
		}
		// Only a clean, explicitly suspended call is eligible. Crashes remain
		// recovery work and cannot be turned into authorization by a hash.
		if run.Status != task.RunWaitingApproval || call.Status != task.ToolCancelled || call.Result.Output != "Awaiting explicit approval; operation not admitted." {
			return fmt.Errorf("approval is not a cleanly suspended operation")
		}
		var detail approvalDetail
		if err := json.Unmarshal(approval.Detail, &detail); err != nil {
			return err
		}
		mode, permissions, generation := e.workspace.policy.snapshot()
		if mode != detail.Mode || permissions != detail.Permissions || generation != detail.Generation {
			return fmt.Errorf("approval policy changed; request a new approval")
		}
		if detail.FullDetailHash == "" {
			return fmt.Errorf("approval has no exact detail binding")
		}
		if detail.MCPServer != "" && (detail.MCPBinding == "" || e.workspace.mcpBindings[detail.MCPServer] != detail.MCPBinding) {
			return fmt.Errorf("MCP connection configuration changed; request a new approval")
		}
		if e.workspace.projectContext != nil {
			for _, path := range detail.ContextPaths {
				if err := e.workspace.projectContext.ObservePath(path); err != nil {
					return err
				}
			}
			snapshot, err := e.workspace.projectContext.Refresh()
			if err != nil {
				return err
			}
			if snapshot.Digest != detail.ContextDigest {
				return fmt.Errorf("project instructions changed; request a new approval")
			}
			if _, err := e.workspace.projectContext.Instructions(); err != nil {
				return err
			}
		}
		if call.Name == "undo_last_change" {
			if err := e.restoreUndoApproval(ctx, approval, detail); err != nil {
				return err
			}
		}
		e.resumeApproval, e.resumeCall = &approval, &call
		return nil
	}
	return fmt.Errorf("no pending approval matches --approve-detail-hash")
}

func (e *taskExecution) matchesApproval(request ApprovalRequest, detail approvalDetail) bool {
	old := e.resumeApproval
	if old == nil || e.current == nil {
		return false
	}
	var recorded approvalDetail
	if json.Unmarshal(old.Detail, &recorded) != nil {
		return false
	}
	// Stored RawMessage JSON is compacted during persistence. The replay uses
	// those exact stored arguments; the original hash remains in old evidence.
	return old.Operation == string(request.Kind) && bytes.Equal(e.resumeCall.Arguments, e.current.Arguments) &&
		e.resumeCall.Name == e.current.Name && bytes.Equal(old.WorkspaceState, request.WorkspaceState) &&
		recorded.Title == detail.Title && recorded.FullDetailHash == detail.FullDetailHash &&
		recorded.Mode == detail.Mode && recorded.Permissions == detail.Permissions &&
		recorded.Generation == detail.Generation && recorded.ContextDigest == detail.ContextDigest &&
		recorded.MCPServer == detail.MCPServer && recorded.MCPBinding == detail.MCPBinding
}
