package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"meldra/internal/provider"
	taskstore "meldra/internal/store"
)

func TestParseFileReferences(t *testing.T) {
	for _, test := range []struct {
		text string
		want []FileReference
		fail bool
	}{
		{`read @src/main.go and @"path with spaces/中文.go":2-4`, []FileReference{{Path: "src/main.go"}, {Path: "path with spaces/中文.go", StartLine: 2, EndLine: 4}}, false},
		{"email name@example.com \\@literal @ ", nil, false},
		{"`@code.go`\n```go\n@fenced.go\n```\n@actual.go:3", []FileReference{{Path: "actual.go", StartLine: 3, EndLine: 3}}, false},
		{"~~~\n@fenced.go\n~~~\n@actual.go.", []FileReference{{Path: "actual.go"}}, false},
		{`@file.go:0`, nil, true}, {`@file.go:5-2`, nil, true}, {`@file.go:2-x`, nil, true}, {`@"missing quote`, nil, true},
	} {
		t.Run(test.text, func(t *testing.T) {
			got, err := ParseFileReferences(test.text)
			if (err != nil) != test.fail {
				t.Fatalf("err=%v", err)
			}
			if !test.fail && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got=%+v want=%+v", got, test.want)
			}
		})
	}
}

func TestResolveReferencesPreservesExactLinesAndProvenance(t *testing.T) {
	w := contextWorkspace(t)
	writeContextFixture(t, w, "file.go", "one\r\ntwo\n三")
	refs, err := w.ResolveReferences("inspect @file.go:2-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Content != "two\n三" || refs[0].ActualStartLine != 2 || refs[0].ActualEndLine != 3 || refs[0].Digest != digest([]byte("two\n三")) {
		t.Fatalf("refs=%+v", refs)
	}
	if _, err := w.ResolveReferences("@file.go:4"); err == nil {
		t.Fatal("out-of-range reference accepted")
	}
	writeContextFixture(t, w, "empty.txt", "")
	empty, err := w.ResolveReferences("@empty.txt")
	if err != nil || len(empty) != 1 || empty[0].ActualEndLine != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	writeContextFixture(t, w, "large.txt", strings.Repeat("line\n", maxReferenceBytes))
	large, err := w.ResolveReferences("@large.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !large[0].Truncated || len(large[0].Content) > maxReferenceBytes {
		t.Fatal("missing bounded truncation")
	}
	for _, path := range []string{"../secret", ".git/config"} {
		if _, err := w.ResolveReferences("@" + path); err == nil {
			t.Fatalf("unsafe %s accepted", path)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w.root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ResolveReferences("@link.txt"); err == nil {
		t.Fatal("symlink reference accepted")
	}
}

func TestReferenceArtifactsSurviveSourceChangeAndRecovery(t *testing.T) {
	a, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	w := a.execution.workspace
	w.projectContext = NewProjectContext(w)
	a.projectContext = w.projectContext
	writeContextFixture(t, w, "source.txt", "original snapshot")
	if err := a.execution.begin(t.Context(), "inspect @source.txt"); err != nil {
		t.Fatal(err)
	}
	input, refs, err := a.prepareReferences(t.Context(), "inspect @source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, "original snapshot") || len(refs) != 1 || refs[0].Artifact.ID == "" {
		t.Fatalf("input=%s refs=%+v", input, refs)
	}
	a.session.appendMessage("user", "inspect @source.txt")
	a.session.Messages[0].References = refs
	if err := a.store.Save(a.session); err != nil {
		t.Fatal(err)
	}
	sequence := a.execution.requestSequence
	writeContextFixture(t, w, "source.txt", "changed on disk")
	history, err := a.referenceHistory(t.Context())
	if err != nil || !strings.Contains(history, "original snapshot") || strings.Contains(history, "changed on disk") {
		t.Fatalf("history=%s err=%v", history, err)
	}
	var recovered SessionMessage
	if err := a.execution.restoreRequestReferences(t.Context(), sequence, &recovered); err != nil {
		t.Fatal(err)
	}
	if len(recovered.References) != 1 || recovered.References[0].Digest != refs[0].Digest {
		t.Fatalf("lost recovered reference: %+v", recovered)
	}
	if err := a.execution.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := newTaskSessionStore(paths).Load(a.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "original snapshot") {
		t.Fatal("attachment bytes flattened into session")
	}
	if len(loaded.Messages[0].References) != 1 {
		t.Fatal("snapshot reference lost")
	}
}

func TestRemotePromptReferenceSyntaxDoesNotReadLocalFiles(t *testing.T) {
	w := contextWorkspace(t)
	a := NewAgent(nil, nil, nil)
	a.projectContext = w.projectContext
	a.suppressNextReferences = true
	input, refs, err := a.prepareReferences(t.Context(), "remote says @missing-secret.txt")
	if err != nil || len(refs) != 0 || input != "remote says @missing-secret.txt" {
		t.Fatalf("remote refs=%v err=%v", refs, err)
	}
	if _, _, err := a.prepareReferences(t.Context(), "user says @missing-secret.txt"); err == nil {
		t.Fatal("suppression leaked into the next user request")
	}
}

func TestControlReferencesFreezeAcceptedBytesWithoutChangingCurrentRules(t *testing.T) {
	a, _ := recordedAgent(t, nil, nil)
	w := a.execution.workspace
	w.projectContext = NewProjectContext(w)
	a.projectContext = w.projectContext
	writeContextFixture(t, w, "nested/AGENTS.md", "nested rules")
	writeContextFixture(t, w, "nested/source.txt", "accepted bytes")
	if _, err := w.projectContext.Instructions(); err != nil {
		t.Fatal(err)
	}
	if err := a.execution.begin(t.Context(), "current work"); err != nil {
		t.Fatal(err)
	}
	defer a.execution.finish(t.Context(), nil)
	_, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	if err := a.beginControl(t.Context(), cancel); err != nil {
		t.Fatal(err)
	}
	defer a.endControl()
	request := ControlRequest{ID: "control-ref", Kind: "queue", Text: "inspect @nested/source.txt"}
	_, refs, err := a.prepareControlReferences(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.References = refs
	if err := w.projectContext.BeforeWrite([]string{"current.txt"}); err != nil {
		t.Fatalf("queue acceptance changed current instruction scope: %v", err)
	}
	writeContextFixture(t, w, "nested/source.txt", "new bytes")
	input, restored, err := a.prepareControlReferences(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, "accepted bytes") || strings.Contains(input, "new bytes") || len(restored) != 1 {
		t.Fatalf("input=%s refs=%v", input, restored)
	}
	if err := w.projectContext.BeforeWrite([]string{"nested/new.txt"}); err == nil {
		t.Fatal("applying accepted references did not discover new scoped instructions")
	}
}

func TestReferenceArtifactQuotaFailureStopsSubmission(t *testing.T) {
	a, paths := recordedAgent(t, nil, nil)
	w := a.execution.workspace
	w.projectContext = NewProjectContext(w)
	a.projectContext = w.projectContext
	writeContextFixture(t, w, "source.txt", "required attachment")
	if err := a.execution.begin(t.Context(), "inspect @source.txt"); err != nil {
		t.Fatal(err)
	}
	defer a.execution.finish(t.Context(), nil)
	dir := filepath.Join(taskDirectory(paths), "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "quota-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(taskstore.MaxArtifactTotalBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	input, refs, err := a.prepareReferences(t.Context(), "inspect @source.txt")
	if err == nil || input != "" || len(refs) != 0 {
		t.Fatalf("failed required snapshot was accepted: refs=%v err=%v", refs, err)
	}
	if a.execution.err == nil {
		t.Fatal("artifact recording failure did not stop execution")
	}
}

func TestQueuedReferenceKeepsAcceptedSnapshotInFollowingRun(t *testing.T) {
	var a *Agent
	requests := 0
	backend := inferenceFunc(func(ctx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 {
			if err := a.SubmitControl("queue", "inspect @source.txt"); err != nil {
				t.Fatal(err)
			}
			writeContextFixture(t, a.execution.workspace, "source.txt", "changed between turns")
		} else if requests == 2 {
			history, err := a.referenceHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(history, "accepted before next run") || strings.Contains(history, "changed between turns") {
				t.Fatalf("next-run attachment=%s", history)
			}
		}
		return provider.Result{Response: finishResponse()}, nil
	})
	a, _ = recordedAgent(t, backend, nil)
	w := a.execution.workspace
	w.projectContext = NewProjectContext(w)
	a.projectContext = w.projectContext
	writeContextFixture(t, w, "source.txt", "accepted before next run")
	provided := false
	a.getUserMessage = func() (string, bool) {
		if provided {
			return "", false
		}
		provided = true
		return "initial task", true
	}
	if err := a.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	var queued *SessionMessage
	for i := range a.session.Messages {
		if a.session.Messages[i].Content == "inspect @source.txt" {
			queued = &a.session.Messages[i]
		}
	}
	if queued == nil || len(queued.References) != 1 || queued.ControlID == "" {
		t.Fatalf("queued snapshot=%+v", queued)
	}
	if queued.References[0].Digest != digest([]byte("accepted before next run")) {
		t.Fatal("queued reference changed at application")
	}
}
