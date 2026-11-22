package harbor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
)

type imageItem struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

const dataPNG = "data:image/png;base64,"
const originalPNG = "f4d0330407b363a9ef03d563e5c2ffd24aa76345f997613741f9fc0935354305"

// ImageReply forms the original high-detail response from bytes actually
// returned by the guest, after validating the reviewed PNG dimensions.
func ImageReply(data []byte) ([]byte, error) {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 1160 || config.Height != 804 || len(data) > 1<<20 || digest(data) != originalPNG {
		return nil, errors.New("unexpected reviewed PNG dimensions or size")
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return json.Marshal([]imageItem{{Type: "input_image", ImageURL: dataPNG + base64.StdEncoding.EncodeToString(data), Detail: "high"}})
}

func recordedImage(c indexedCall) (imageItem, error) {
	url, ok := strings.CutPrefix(c.output, "[{'type': 'input_image', 'image_url': '")
	if !ok {
		return imageItem{}, errors.New("unexpected ATIF image attachment")
	}
	url, ok = strings.CutSuffix(url, "', 'detail': 'high'}]")
	if !ok || !strings.HasPrefix(url, dataPNG) {
		return imageItem{}, errors.New("unexpected image MIME or detail")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, dataPNG))
	if err != nil || digest(raw) != originalPNG {
		return imageItem{}, errors.New("unexpected source image bytes")
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
	if err := d.Decode(&args); err != nil || args.Path != "/app/code.png" {
		return replay.Call{}, errors.New("unexpected reviewed image path")
	}
	item, err := recordedImage(c)
	if err != nil {
		return replay.Call{}, err
	}
	wire, err := json.Marshal([]imageItem{item})
	return replay.Call{Operation: "ViewImage", Path: args.Path, Sequence: seq, StartOffset: ts.Sub(origin), ToolCallID: c.call.ID, ResponseStdout: wire}, err
}
