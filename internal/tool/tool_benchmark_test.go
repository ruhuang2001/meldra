package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func BenchmarkRegistryDispatch(b *testing.B) {
	for _, count := range []int{1, 16, 128, 512} {
		b.Run(fmt.Sprintf("tools_%d", count), func(b *testing.B) {
			definitions := make([]Definition, count)
			for i := range definitions {
				definitions[i] = Definition{Name: fmt.Sprintf("tool_%d", i), Function: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
			}
			registry, err := New(definitions)
			if err != nil {
				b.Fatal(err)
			}
			name := definitions[count-1].Name
			input := json.RawMessage(`{}`)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := registry.Execute(b.Context(), name, input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
