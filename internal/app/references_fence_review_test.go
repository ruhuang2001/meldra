package app

import (
	"reflect"
	"testing"
)

func TestFileReferencesRespectFenceClosingDelimiter(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
	}{
		{"shorter backtick run", "````markdown\n```go\n@inside.go\n```\n````\n@outside.go"},
		{"shorter tilde run", "~~~~markdown\n~~~\n@inside.go\n~~~\n~~~~\n@outside.go"},
		{"longer backtick closer", "````markdown\n@inside.go\n`````\n@outside.go"},
		{"longer tilde closer", "~~~~markdown\n@inside.go\n~~~~~\n@outside.go"},
		{"backtick closer with text", "```markdown\n``` text\n@inside.go\n```\n@outside.go"},
		{"tilde closer with text", "~~~markdown\n~~~ text\n@inside.go\n~~~\n@outside.go"},
		{"different delimiter", "````markdown\n~~~~\n@inside.go\n````\n@outside.go"},
		{"whitespace after closer", "````markdown\n@inside.go\n```` \t\r\n@outside.go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			refs, err := ParseFileReferences(test.text)
			want := []FileReference{{Path: "outside.go"}}
			if err != nil || !reflect.DeepEqual(refs, want) {
				t.Fatalf("refs=%+v want=%+v error=%v", refs, want, err)
			}
		})
	}
}
