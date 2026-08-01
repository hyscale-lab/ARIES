package audio

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestReadWAVPCM16MonoAcceptsChunkedPCM(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0x02, 0x00, 0xff, 0x7f, 0x00, 0x80}
	wav := testWAV(t, 24000, 1, 16, 1, pcm, true)

	got, rate, err := ReadWAVPCM16Mono(bytes.NewReader(wav))
	if err != nil {
		t.Fatalf("ReadWAVPCM16Mono returned error: %v", err)
	}
	if rate != 24000 {
		t.Fatalf("rate = %d, want 24000", rate)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("pcm = %v, want %v", got, pcm)
	}
}

func TestReadWAVPCM16MonoAcceptsUnknownStreamingSizes(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x00}
	wav := testWAVWithUnknownSizes(t, 24000, pcm)

	got, rate, err := ReadWAVPCM16Mono(bytes.NewReader(wav))
	if err != nil {
		t.Fatalf("ReadWAVPCM16Mono returned error: %v", err)
	}
	if rate != 24000 {
		t.Fatalf("rate = %d, want 24000", rate)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("pcm = %v, want %v", got, pcm)
	}
}

func TestReadWAVPCM16MonoRejectsUnsupportedFormats(t *testing.T) {
	tests := []struct {
		name string
		wav  []byte
		want string
	}{
		{name: "compressed", wav: testWAV(t, 24000, 1, 16, 7, []byte{0, 0}, false), want: "PCM"},
		{name: "stereo", wav: testWAV(t, 24000, 2, 16, 1, []byte{0, 0, 0, 0}, false), want: "mono"},
		{name: "eight bit", wav: testWAV(t, 24000, 1, 8, 1, []byte{0}, false), want: "16-bit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ReadWAVPCM16Mono(bytes.NewReader(test.wav))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestSilencePCM16(t *testing.T) {
	got, err := SilencePCM16(24000, 50)
	if err != nil {
		t.Fatalf("SilencePCM16 returned error: %v", err)
	}
	if len(got) != 2400 {
		t.Fatalf("len = %d, want 2400", len(got))
	}
	if !bytes.Equal(got, make([]byte, 2400)) {
		t.Fatalf("silence contains nonzero bytes")
	}
}

func TestResamplePCM16PreservesEndpointsAndRejectsInvalidInput(t *testing.T) {
	input := pcm16Bytes(-32768, -16384, 0, 16384, 32767)
	got, err := ResamplePCM16(input, 5, 3)
	if err != nil {
		t.Fatalf("ResamplePCM16 returned error: %v", err)
	}
	want := pcm16Bytes(-32768, 0, 32767)
	if !bytes.Equal(got, want) {
		t.Fatalf("resampled = %v, want %v", decodePCM16(t, got), decodePCM16(t, want))
	}

	if _, err := ResamplePCM16([]byte{1}, 24000, 16000); err == nil || !strings.Contains(err.Error(), "odd") {
		t.Fatalf("odd input error = %v, want odd length rejection", err)
	}
}

func TestPCM16ToG711ULawKnownPoints(t *testing.T) {
	got, err := PCM16ToG711ULaw(pcm16Bytes(-32768, 0, 32767))
	if err != nil {
		t.Fatalf("PCM16ToG711ULaw returned error: %v", err)
	}
	want := []byte{0x00, 0xff, 0x80}
	if !bytes.Equal(got, want) {
		t.Fatalf("ulaw = %#v, want %#v", got, want)
	}
}

func TestPrepareAudioResamplesAndEncodes(t *testing.T) {
	prepared, err := PrepareAudio(pcm16Bytes(0, 32767), 2, "mulaw", 1)
	if err != nil {
		t.Fatalf("PrepareAudio returned error: %v", err)
	}
	if prepared.Rate != 1 || prepared.BytesPerSample != 1 || prepared.Encoding != "g711_ulaw" {
		t.Fatalf("prepared metadata = %#v", prepared)
	}
	if !bytes.Equal(prepared.Data, []byte{0xff}) {
		t.Fatalf("prepared data = %#v, want %#v", prepared.Data, []byte{0xff})
	}
}

func TestAudioBoundsRejectHugeRatesOutputsAndSilence(t *testing.T) {
	if _, err := ResamplePCM16(pcm16Bytes(1), MaxSampleRate+1, 24000); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("huge source rate error = %v", err)
	}
	// A small input can still imply an enormous output when metadata claims a
	// one-hertz source. Reject it before allocating the derived output buffer.
	if _, err := ResamplePCM16(make([]byte, 200), 1, MaxSampleRate); err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("huge resample output error = %v", err)
	}
	if _, err := SilencePCM16(MaxSampleRate, MaxSilenceDurationMillis+1); err == nil || !strings.Contains(err.Error(), "duration") {
		t.Fatalf("huge silence error = %v", err)
	}
	wav := testWAV(t, MaxSampleRate+1, 1, 16, 1, []byte{0, 0}, false)
	if _, _, err := ReadWAVPCM16Mono(bytes.NewReader(wav)); err == nil || !strings.Contains(err.Error(), "sample rate") {
		t.Fatalf("huge WAV rate error = %v", err)
	}
}

func pcm16Bytes(values ...int16) []byte {
	var out bytes.Buffer
	for _, value := range values {
		_ = binary.Write(&out, binary.LittleEndian, value)
	}
	return out.Bytes()
}

func decodePCM16(t *testing.T, content []byte) []int16 {
	t.Helper()
	if len(content)%2 != 0 {
		t.Fatalf("odd pcm length %d", len(content))
	}
	out := make([]int16, len(content)/2)
	for index := range out {
		out[index] = int16(binary.LittleEndian.Uint16(content[index*2:]))
	}
	return out
}

func testWAV(t *testing.T, rate, channels, bits, format int, pcm []byte, withJunk bool) []byte {
	t.Helper()
	var chunks bytes.Buffer
	writeChunk := func(id string, payload []byte) {
		chunks.WriteString(id)
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(payload)))
		chunks.Write(payload)
		if len(payload)%2 != 0 {
			chunks.WriteByte(0)
		}
	}
	var fmtChunk bytes.Buffer
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(format))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint32(rate))
	byteRate := rate * channels * bits / 8
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint32(byteRate))
	blockAlign := channels * bits / 8
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(bits))
	writeChunk("fmt ", fmtChunk.Bytes())
	if withJunk {
		writeChunk("JUNK", []byte{1, 2, 3})
	}
	writeChunk("data", pcm)

	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(4+chunks.Len()))
	out.WriteString("WAVE")
	out.Write(chunks.Bytes())
	return out.Bytes()
}

func testWAVWithUnknownSizes(t *testing.T, rate int, pcm []byte) []byte {
	t.Helper()
	var fmtChunk bytes.Buffer
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(1))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(1))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(2))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(16))

	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(wavUnknownSize))
	out.WriteString("WAVE")
	out.WriteString("fmt ")
	_ = binary.Write(&out, binary.LittleEndian, uint32(fmtChunk.Len()))
	out.Write(fmtChunk.Bytes())
	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, uint32(wavUnknownSize))
	out.Write(pcm)
	return out.Bytes()
}
