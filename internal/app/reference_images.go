package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"meldra/internal/provider"
)

const (
	maxImageReferenceTotalBytes = 2 << 20
	maxImagePixels              = 16_000_000
)

func referenceMIME(data []byte, path string) (string, error) {
	imageFile := slices.Contains([]string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".avif", ".bmp", ".ico", ".tif", ".tiff"}, strings.ToLower(filepath.Ext(path)))
	png := bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	jpeg := bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff})
	if !png && !jpeg {
		if imageFile || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 || binarySample(data) {
			return "", fmt.Errorf("reference must contain UTF-8 text or a supported PNG/JPEG image")
		}
		return "", nil
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("invalid image header: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxImagePixels/config.Height {
		return "", fmt.Errorf("image exceeds %d decoded pixels", maxImagePixels)
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("invalid image data: %w", err)
	}
	switch format {
	case "png":
		return "image/png", nil
	case "jpeg":
		return "image/jpeg", nil
	default:
		return "", fmt.Errorf("unsupported image format %q", format)
	}
}

func (r ReferenceSnapshot) bytes() []byte {
	if r.MIMEType != "" {
		return r.ImageData
	}
	return []byte(r.Content)
}

func (r ReferenceSnapshot) artifactName(prefix string) string {
	switch r.MIMEType {
	case "image/png":
		return prefix + ".png"
	case "image/jpeg":
		return prefix + ".jpg"
	default:
		return prefix + ".txt"
	}
}

func (r *ReferenceSnapshot) restoreBytes(data []byte) {
	if r.MIMEType != "" {
		r.ImageData = nil
	} else {
		r.Content = string(data)
	}
}

// userInferenceInput reconstructs images from accepted immutable references.
// Base64 exists only in this transient provider input, never durable messages.
func (a *Agent) userInferenceInput(ctx context.Context, text string, current []ReferenceSnapshot) (provider.Input, provider.Items, error) {
	refs := current
	if a.session != nil {
		refs = nil
		for i := len(a.session.Messages) - 1; i >= 0; i-- {
			refs = append(refs, a.session.Messages[i].References...)
		}
	}
	total, omitted := 0, 0
	seen := map[string]bool{}
	var images []provider.ImageContent
	var retained []ReferenceSnapshot
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return provider.Input{}, nil, err
		}
		if ref.MIMEType == "" || seen[ref.Digest] {
			continue
		}
		seen[ref.Digest] = true
		size := len(ref.ImageData)
		if ref.Artifact.ID != "" {
			size = int(ref.Artifact.Size)
		}
		if size > maxImageReferenceTotalBytes-total || len(images) >= maxReferences {
			omitted++
			continue
		}
		data := ref.ImageData
		if ref.Artifact.ID != "" {
			if a.execution == nil || a.execution.db == nil {
				return provider.Input{}, nil, fmt.Errorf("image references require task artifact storage")
			}
			var err error
			data, err = a.execution.db.ReadArtifact(ctx, ref.Artifact)
			if err != nil {
				return provider.Input{}, nil, fmt.Errorf("restore image reference %s: %w", ref.Path, err)
			}
		}
		if digest(data) != ref.Digest {
			return provider.Input{}, nil, fmt.Errorf("image reference snapshot digest mismatch")
		}
		mimeType, err := referenceMIME(data, ref.Path)
		if err != nil || mimeType != ref.MIMEType {
			return provider.Input{}, nil, fmt.Errorf("invalid stored image reference %s", ref.Path)
		}
		images = append(images, provider.ImageContent{URL: "data:" + ref.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(data)})
		retained = append(retained, ref)
		total += len(data)
	}
	if omitted > 0 {
		text += fmt.Sprintf("\n[%d older image snapshots omitted by the %d byte / 32 image history budget; original artifacts remain in task history.]\n", omitted, maxImageReferenceTotalBytes)
	}
	// Declare current and historical images together, after selection, so each
	// retained digest has one declaration in the same order as its image payload.
	if len(images) == 0 {
		return provider.UserInput(text), provider.UserItems(text), nil
	}
	text += "\nImage content follows in the order of these immutable image references:" + referenceInput(retained)
	items, err := provider.UserItemsWithImages(text, images)
	if err != nil {
		return provider.Input{}, nil, err
	}
	return provider.ItemsInput(items), items, nil
}
