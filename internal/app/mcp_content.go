package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"meldra/internal/task"
	"meldra/internal/tool"
)

const mcpContentManifest = "mcp-content-v1.json"

func mcpToolResult(ctx context.Context, result *mcp.CallToolResult) (string, error) {
	clean := *result
	clean.Content = slices.Clone(result.Content)
	var parts []tool.ContentPart
	var images []tool.OutputArtifact
	total, textBytes := 0, 0
	omitted := false
	addText := func(text string) {
		remaining := maxToolOutput - textBytes
		if len(text) > remaining {
			omitted = true
			text = truncateUTF8(text, remaining, " [text omitted]")
		}
		if text != "" {
			parts = append(parts, tool.ContentPart{Text: text})
			textBytes += len(text)
		}
	}
	for i, content := range result.Content {
		image, ok := content.(*mcp.ImageContent)
		if !ok {
			if text, ok := content.(*mcp.TextContent); ok {
				addText(text.Text)
			} else {
				encoded, err := json.Marshal(content)
				if err != nil {
					return "", err
				}
				addText(string(encoded))
			}
			continue
		}
		reason := ""
		if len(image.Data) > 1<<20 || len(image.Data) > maxImageReferenceTotalBytes-total || len(images) >= maxReferences {
			reason = "image byte/count limit exceeded; request a smaller viewport or element screenshot"
		} else if image.MIMEType != "image/png" && image.MIMEType != "image/jpeg" {
			reason = "only inline PNG/JPEG images are supported"
		} else if mime, err := referenceMIME(image.Data, "screenshot.png"); err != nil || mime != image.MIMEType {
			reason = "invalid image bytes or MIME mismatch"
		}
		if reason != "" {
			marker := fmt.Sprintf("[Image %d not provided to model: %s.]", i+1, reason)
			clean.Content[i] = &mcp.TextContent{Text: marker}
			addText(marker)
			omitted = true
			continue
		}
		ext := "png"
		if image.MIMEType == "image/jpeg" {
			ext = "jpg"
		}
		name := fmt.Sprintf("mcp-image-%d.%s", i+1, ext)
		data := slices.Clone(image.Data)
		parts = append(parts, tool.ContentPart{Image: &tool.OutputImage{Name: name, MIMEType: image.MIMEType, Data: data}})
		images = append(images, tool.OutputArtifact{Name: name, Content: data})
		total += len(data)
		clean.Content[i] = &mcp.TextContent{Text: fmt.Sprintf("[Image artifact: %s; sha256=%s; %d bytes. Historical capture, not live page state.]", name, digest(data), len(data))}
	}
	data, err := json.Marshal(&clean)
	if err != nil {
		return "", err
	}
	truncated := len(data) > maxMCPMessage
	if truncated {
		data = []byte(truncateUTF8(string(data), maxMCPMessage, "\n[artifact truncated]"))
	}
	attachments := []tool.OutputArtifact{{Name: "mcp-result.json", Content: data, Truncated: truncated}}
	if len(images) > 0 {
		metadata := clean
		metadata.Content = nil
		encoded, err := json.Marshal(&metadata)
		if err != nil {
			return "", err
		}
		addText("MCP result metadata: " + string(encoded))
		if omitted {
			parts = append(parts, tool.ContentPart{Text: "[Some MCP content was omitted by validation or size limits; do not claim it was inspected.]"})
		}
		manifest, err := json.Marshal(parts)
		if err != nil {
			return "", err
		}
		attachments = append(attachments, images...)
		attachments = append(attachments, tool.OutputArtifact{Name: mcpContentManifest, Content: manifest})
	} else {
		parts = nil
	}
	output := capText(string(data))
	tool.Observe(ctx, func(o *tool.Observation) {
		o.Result.Status = tool.Succeeded
		if result.IsError {
			o.Result.Status = tool.Failed
		}
		o.Result.Content = parts
		o.Result.Attachments = attachments
		o.Result.Truncated = omitted || truncated || len(data) > maxToolOutput
	})
	if result.IsError {
		return output, fmt.Errorf("MCP tool failed: %s", output)
	}
	return output, nil
}

func (e *taskExecution) restoreMCPContent(ctx context.Context, result task.Result) ([]tool.ContentPart, error) {
	if result.ContentManifest == "" {
		return nil, nil
	}
	if result.ContentManifest != mcpContentManifest {
		return nil, fmt.Errorf("unsupported MCP content manifest %q", result.ContentManifest)
	}
	refs := result.Artifacts
	byName := make(map[string]task.ArtifactRef, len(refs))
	for _, ref := range refs {
		byName[ref.Name] = ref
	}
	manifest, ok := byName[mcpContentManifest]
	if !ok {
		return nil, fmt.Errorf("missing MCP content manifest; image evidence was not saved")
	}
	data, err := e.db.ReadArtifact(ctx, manifest)
	if err != nil {
		return nil, err
	}
	var parts []tool.ContentPart
	if err := json.Unmarshal(data, &parts); err != nil {
		return nil, err
	}
	total, count := 0, 0
	for i := range parts {
		image := parts[i].Image
		if image == nil {
			continue
		}
		ref, ok := byName[image.Name]
		if !ok || ref.Size > 1<<20 || ref.Size > int64(maxImageReferenceTotalBytes-total) || count >= maxReferences {
			return nil, fmt.Errorf("missing or oversized MCP image artifact")
		}
		image.Data, err = e.db.ReadArtifact(ctx, ref)
		if err != nil {
			return nil, err
		}
		mime, err := referenceMIME(image.Data, image.Name)
		if err != nil || mime == "" || mime != image.MIMEType {
			return nil, fmt.Errorf("invalid stored MCP image")
		}
		total += len(image.Data)
		count++
	}
	return parts, nil
}
