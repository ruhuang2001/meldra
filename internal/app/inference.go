package app

import (
	"context"
	"fmt"

	"meldra/internal/provider"
)

func (a *Agent) runInference(ctx context.Context, input provider.Input, previousResponseID string) (provider.Result, error) {
	mode, permissions, _ := a.policy.snapshot()
	tools := make([]provider.Tool, 0, len(a.tools))
	for _, tool := range a.tools {
		if mode == ModePlan {
			class := classifyOperation(tool.Name, nil)
			if class != operationRead && class != operationMetadata && class != operationControl && tool.Name != "verify" {
				continue
			}
		}
		tools = append(tools, provider.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, NonStrict: tool.NonStrict})
	}
	if a.backend == nil {
		a.backend = &provider.Client{}
	}
	instructions := agentInstructions + fmt.Sprintf("\nRuntime mode: %s. Permission profile: %s. Only the user can change these settings.\n", mode, permissions)
	if mode == ModePlan {
		instructions += "Plan mode: investigate and propose a plan. Do not modify the project or execute project programs or remote actions. You may save plan/summary metadata. Request a user switch to Build before implementation. The verify tool is restricted to preset diff.\n"
	} else {
		instructions += "Build mode: follow the user request within the active permission profile; changing mode does not preapprove commands.\n"
	}
	if a.skills != nil {
		instructions += a.skills.instructions()
	}
	if a.projectContext != nil {
		projectInstructions, err := a.projectContext.Instructions()
		if err != nil {
			return provider.Result{}, err
		}
		instructions += projectInstructions
		if a.execution != nil {
			if err := a.execution.persistProjectContext(ctx, a.projectContext); err != nil {
				return provider.Result{}, err
			}
		}
	}
	return a.backend.Infer(ctx, provider.Request{Model: a.modelName(), Instructions: instructions, Input: input, PreviousResponseID: previousResponseID, Tools: tools},
		provider.Options{CustomProvider: a.customProvider, IdleTimeout: a.streamIdleTimeout, MaxResponseBytes: a.maxProviderResponseBytes},
		provider.Observer{Status: func(text string) { a.emit(UIEvent{Kind: UIEventStatus, Text: text}) }, Text: a.emitAssistantDelta, FilterText: sanitizeTerminalText})
}
