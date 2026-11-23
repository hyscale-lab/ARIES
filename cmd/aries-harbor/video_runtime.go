package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

const videoIPPDisabledTrial = "aa067203-b539-48d0-9c99-2128f941c9d0"

func validateVideoIPP(recording *harbor.Recording, profile string) error {
	if profile != "" && (recording.Task != "video-processing" || recording.Trial != videoIPPDisabledTrial || profile != "disabled") {
		return fmt.Errorf("OpenCV IPP profile %q is unavailable for %s trial %s", profile, recording.Task, recording.Trial)
	}
	return nil
}

// Read back the environment inherited by all original guest commands. The
// profile is bound to a reviewed trial with an exact JPEG-producing path.
const videoRuntimeProbe = `import cv2, hashlib, json, numpy, os, platform, sys
profile = os.environ.get('OPENCV_IPP')
assert profile == sys.argv[1] == 'disabled' and not cv2.ipp.useIPP()
assert (cv2.__version__, numpy.__version__, platform.python_version()) == ('5.0.0', '2.5.2', '3.13.7')
video = cv2.VideoCapture('/app/example_video.mp4')
assert video.isOpened() and video.getBackendName() == 'FFMPEG'
print(json.dumps({'profile': profile, 'ipp_enabled': cv2.ipp.useIPP(), 'ipp_inexact': cv2.ipp.useIPP_NotExact(),
    'opencv': cv2.__version__, 'numpy': numpy.__version__, 'python': platform.python_version(),
    'cpu_features': cv2.getCPUFeaturesLine(), 'video_backend': video.getBackendName(),
    'opencv_build_sha256': hashlib.sha256(cv2.getBuildInformation().encode()).hexdigest()}))
video.release()`

func verifyVideoRuntime(ctx context.Context, manager *vhive.Manager, id, profile, phase string, log *json.Encoder) error {
	if profile == "" {
		return nil
	}
	output, err := checked(ctx, manager, id, core.Command{Path: "/usr/local/bin/python3", Args: []string{"-c", videoRuntimeProbe, profile}})
	if err != nil {
		return fmt.Errorf("verify OpenCV runtime %s: %w", phase, err)
	}
	if !json.Valid([]byte(output)) {
		return fmt.Errorf("invalid OpenCV runtime readback %s", phase)
	}
	return log.Encode(map[string]any{"event": "video_opencv_runtime", "phase": phase, "runtime": json.RawMessage(output)})
}
