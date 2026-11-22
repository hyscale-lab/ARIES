package harbor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"path"

	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
)

type imageItem struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

func recordedImage(c indexedCall) (imageItem, error) {
	url, ok := strings.CutPrefix(c.output, "[{'type': 'input_image', 'image_url': '")
	if !ok {
		return imageItem{}, errors.New("unexpected ATIF image attachment")
	}
	url, ok = strings.CutSuffix(url, "', 'detail': 'high'}]")
	if !ok || (!strings.HasPrefix(url, "data:image/png;base64,") && !strings.HasPrefix(url, "data:image/jpeg;base64,")) {
		return imageItem{}, errors.New("unexpected image MIME or detail")
	}
	if len(url) > 12<<20 {
		return imageItem{}, errors.New("source attachment exceeds supported size")
	}
	return imageItem{Type: "input_image", ImageURL: url, Detail: "high"}, nil
}

func matchesReply(c indexedCall, raw json.RawMessage) bool {
	if c.call.Name != "view_image" {
		var str string
		return json.Unmarshal(raw, &str) == nil && str == c.output
	}
	want, err := recordedImage(c)
	var items []imageItem
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return err == nil && d.Decode(&items) == nil && len(items) == 1 && items[0] == want
}

func imageCall(c indexedCall, origin, ts time.Time, seq uint64) (replay.Call, error) {
	var args struct {
		Path string `json:"path"`
	}
	d := json.NewDecoder(bytes.NewReader(c.call.Args))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil || path.Clean(args.Path) != args.Path || (!strings.HasPrefix(args.Path, "/app/") && !strings.HasPrefix(args.Path, "/tmp/")) {
		return replay.Call{}, errors.New("unexpected reviewed image path")
	}
	item, err := recordedImage(c)
	if err != nil {
		return replay.Call{}, err
	}
	prefix, encoded, _ := strings.Cut(item.ImageURL, ",")
	cfg, format, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)))
	if err != nil || prefix != "data:image/"+format+";base64" || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 {
		return replay.Call{}, errors.New("invalid original image attachment")
	}
	wire, err := json.Marshal([]imageItem{item})
	return replay.Call{Operation: "ViewImage", ImageWidth: cfg.Width, ImageHeight: cfg.Height, Path: args.Path, Sequence: seq, StartOffset: ts.Sub(origin), ToolCallID: c.call.ID, ResponseStdout: wire}, err
}

// Check live bytes are an actual image of the source shape and MIME before
// continuing a video-only diagnostic that preserves the divergent attachment.
func comparableLiveImage(call replay.Call, wire string) bool {
	var actual, original []imageItem
	if json.Unmarshal([]byte(wire), &actual) != nil || json.Unmarshal(call.ResponseStdout, &original) != nil || len(actual) != 1 || len(original) != 1 {
		return false
	}
	prefix, encoded, ok := strings.Cut(actual[0].ImageURL, ",")
	originalPrefix, _, _ := strings.Cut(original[0].ImageURL, ",")
	if !ok || len(encoded) > 12<<20 || prefix != originalPrefix || actual[0].Type != original[0].Type || actual[0].Detail != original[0].Detail {
		return false
	}
	decoded, format, err := image.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)))
	return err == nil && "data:image/"+format+";base64" == prefix && decoded.Bounds().Dx() == call.ImageWidth && decoded.Bounds().Dy() == call.ImageHeight
}
