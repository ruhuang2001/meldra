package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type Definition struct {
	Name        string
	Description string
	Parameters  map[string]any
	NonStrict   bool
	Function    func(context.Context, json.RawMessage) (string, error)
}

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

func (r *Registry) Invoke(ctx context.Context, name string, input json.RawMessage) (Result, error) {
	started := time.Now()
	observation := &Observation{}
	ctx = context.WithValue(ctx, observationKey{}, observation)
	output, err := r.Execute(ctx, name, input)
	return resultFor(ctx, started, observation, output, err)
}
