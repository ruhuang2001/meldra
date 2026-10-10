package app

import (
	"context"

	"meldra/internal/provider"
)

func (a *Agent) runInference(ctx context.Context, input provider.Input, previousResponseID string) (provider.Result, error) {
	tools := make([]provider.Tool, 0, len(a.tools))
	for _, tool := range a.tools {
		tools = append(tools, provider.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, NonStrict: tool.NonStrict})
	}
	if a.backend == nil {
		a.backend = &provider.Client{}
	}
	instructions := agentInstructions
	if a.skills != nil {
		instructions += a.skills.instructions()
	}
	return a.backend.Infer(ctx, provider.Request{Model: a.modelName(), Instructions: instructions, Input: input, PreviousResponseID: previousResponseID, Tools: tools},
		provider.Options{CustomProvider: a.customProvider, IdleTimeout: a.streamIdleTimeout, MaxResponseBytes: a.maxProviderResponseBytes},
		provider.Observer{Status: func(text string) { a.emit(UIEvent{Kind: UIEventStatus, Text: text}) }, Text: a.emitAssistantDelta, FilterText: sanitizeTerminalText})
}
