// Package tool defines the execution contract shared by built-in and future
// external tools. It has no dependency on a model SDK or user interface.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Definition is immutable after registration. Context carries cancellation and
// deadlines for this invocation rather than for the lifetime of a workspace.
type Definition struct {
	Name        string
	Description string
	Parameters  map[string]any
	NonStrict   bool // External schemas retain their optional properties.
	Function    func(context.Context, json.RawMessage) (string, error)
}

// Registry validates tool identity once and dispatches calls by exact name.
// It does not make handlers concurrent-safe or retry side effects.
type Registry struct{ tools map[string]Definition }

func New(definitions []Definition) (*Registry, error) {
	r := &Registry{tools: make(map[string]Definition, len(definitions))}
	for _, definition := range definitions {
		if definition.Name == "" {
			return nil, fmt.Errorf("tool name must not be empty")
		}
		if definition.Function == nil {
			return nil, fmt.Errorf("tool %q has no handler", definition.Name)
		}
		if _, exists := r.tools[definition.Name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", definition.Name)
		}
		r.tools[definition.Name] = definition
	}
	return r, nil
}

func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	definition, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("tool %q not found", name)
	}
	return definition.Function(ctx, input)
}

// Invoke is the structured runtime contract. Execute remains the text adapter
// for callers that do not need execution metadata.
func (r *Registry) Invoke(ctx context.Context, name string, input json.RawMessage) (Result, error) {
	started := time.Now()
	observation := &Observation{}
	ctx = context.WithValue(ctx, observationKey{}, observation)
	output, err := r.Execute(ctx, name, input)
	return resultFor(ctx, started, observation, output, err)
}
