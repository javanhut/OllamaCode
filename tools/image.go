package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MaxImageBytes caps one image file. Vision encoders downscale anyway, so a
// larger file only costs memory and request size.
const MaxImageBytes = 10 << 20

// imageTypes are the formats both Ollama's vision models and OpenAI-compatible
// providers accept, keyed by the media type http.DetectContentType reports.
var imageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// IsImagePath reports whether p names an image file by its extension.
func IsImagePath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// LoadImage reads an image inside the workspace and returns it base64-encoded,
// with its sniffed media type and size. The content decides the type, not the
// extension.
func LoadImage(p string) (b64, mime string, size int, err error) {
	if err := jailCheck(p); err != nil {
		return "", "", 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", "", 0, err
	}
	if info.IsDir() {
		return "", "", 0, fmt.Errorf("%s is a directory", p)
	}
	if info.Size() > MaxImageBytes {
		return "", "", 0, fmt.Errorf("%s is %d bytes; images are limited to %d", p, info.Size(), MaxImageBytes)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", "", 0, err
	}
	mime = http.DetectContentType(data)
	if !imageTypes[mime] {
		return "", "", 0, fmt.Errorf("%s is %s, not a PNG, JPEG, GIF or WebP image", p, mime)
	}
	return base64.StdEncoding.EncodeToString(data), mime, len(data), nil
}

// ImagePathArg extracts read_image's path argument.
func ImagePathArg(args json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Path
}

// ReadImageTool validates an image and describes it. The image itself cannot
// travel in a string result: the caller attaches it to the tool message (see
// agent.Executor), which is why only vision-capable models are offered this.
func ReadImageTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "read_image",
			Description: "Look at an image file (PNG, JPEG, GIF or WebP) in the workspace: a screenshot, diagram, mockup or chart. The image is attached to the tool result for you to see. Use read_file for text files.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the image file."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			p := ImagePathArg(args)
			if p == "" {
				return "", fmt.Errorf("path is required")
			}
			_, mime, size, err := LoadImage(p)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("image %s (%s, %d KB) is attached to this result", p, mime, (size+1023)/1024), nil
		},
	}
}
