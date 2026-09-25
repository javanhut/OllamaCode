package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPNG writes a 2x2 PNG to dir/name and returns its path.
func writeTestPNG(t *testing.T, dir, name string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadImageAcceptsPNG(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	p := writeTestPNG(t, root, "shot.png")

	b64, mime, size, err := LoadImage(p)
	if err != nil || mime != "image/png" || size == 0 {
		t.Fatalf("mime=%q size=%d err=%v", mime, size, err)
	}
	if raw, err := base64.StdEncoding.DecodeString(b64); err != nil || !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		t.Fatalf("payload is not the PNG base64-encoded: %v", err)
	}
}

func TestLoadImageRejectsTextWithImageExtension(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	p := filepath.Join(root, "fake.png")
	os.WriteFile(p, []byte("not an image"), 0o644)

	if _, _, _, err := LoadImage(p); err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Fatalf("err = %v, want a type rejection", err)
	}
}

func TestLoadImageStaysInTheJail(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	outside := writeTestPNG(t, t.TempDir(), "x.png")

	if _, _, _, err := LoadImage(outside); err == nil {
		t.Fatal("an image outside the workspace was loaded")
	}
}

func TestReadImageToolDescribesImage(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	writeTestPNG(t, root, "shot.png")

	out, err := ReadImageTool().Handler(context.Background(), json.RawMessage(`{"path":"shot.png"}`))
	if err != nil || !strings.Contains(out, "image/png") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
