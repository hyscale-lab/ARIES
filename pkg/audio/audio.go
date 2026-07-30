package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

const (
	wavPCMFormat          = 1
	wavUnknownSize        = 0xffffffff
	wavHeaderSize         = 12
	wavChunkHeaderSize    = 8
	wavFormatChunkMinSize = 16

	wavRIFFChunk = "RIFF"
	wavWAVEType  = "WAVE"
	wavFmtChunk  = "fmt "
	wavDataChunk = "data"

	audioEncodingPCM16    = "pcm16"
	audioEncodingG711ULaw = "g711_ulaw"
	pcm16BytesPerSample   = 2
	ulawBytesPerSample    = 1

	ulawBias = 0x84
	ulawClip = 32635
	minPCM16 = -32768
	maxPCM16 = 32767
)

var maxInt = int(^uint(0) >> 1)

// PreparedAudio is the gateway-ready audio payload and its wire format.
type PreparedAudio struct {
	Data           []byte
	Rate           int
	BytesPerSample int
	Encoding       string
}

// ReadWAVFilePCM16Mono reads a PCM16 mono WAV file.
func ReadWAVFilePCM16Mono(path string) ([]byte, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	return ReadWAVPCM16Mono(file)
}

// ReadWAVPCM16Mono reads a RIFF/WAVE stream and returns its raw little-endian
// PCM16 mono data plus sample rate.
func ReadWAVPCM16Mono(reader io.Reader) ([]byte, int, error) {
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("read WAV: %w", err)
	}
	if len(content) < wavHeaderSize || string(content[0:4]) != wavRIFFChunk || string(content[8:12]) != wavWAVEType {
		return nil, 0, errors.New("WAV must be RIFF/WAVE")
	}
	riffSize := binary.LittleEndian.Uint32(content[4:8])
	if riffSize != wavUnknownSize && uint64(riffSize)+8 > uint64(len(content)) {
		return nil, 0, errors.New("WAV RIFF size exceeds input")
	}

	var (
		formatSeen bool
		dataSeen   bool
		format     uint16
		channels   uint16
		rate       uint32
		bits       uint16
		data       []byte
	)
	for offset := wavHeaderSize; offset+wavChunkHeaderSize <= len(content); {
		id, payload, nextOffset, err := readWAVChunk(content, offset)
		if err != nil {
			return nil, 0, err
		}
		switch id {
		case wavFmtChunk:
			if len(payload) < wavFormatChunkMinSize {
				return nil, 0, errors.New("WAV fmt chunk is too short")
			}
			formatSeen = true
			format = binary.LittleEndian.Uint16(payload[0:2])
			channels = binary.LittleEndian.Uint16(payload[2:4])
			rate = binary.LittleEndian.Uint32(payload[4:8])
			bits = binary.LittleEndian.Uint16(payload[14:16])
		case wavDataChunk:
			dataSeen = true
			data = append([]byte(nil), payload...)
		}
		offset = nextOffset
	}
	if !formatSeen {
		return nil, 0, errors.New("WAV fmt chunk is missing")
	}
	if format != wavPCMFormat {
		return nil, 0, fmt.Errorf("WAV must be PCM format 1, got %d", format)
	}
	if channels != 1 {
		return nil, 0, fmt.Errorf("WAV must be mono, got %d channel(s)", channels)
	}
	if bits != 16 {
		return nil, 0, fmt.Errorf("WAV must be 16-bit PCM, got %d-bit", bits)
	}
	if rate == 0 || rate > uint32(maxInt) {
		return nil, 0, errors.New("WAV sample rate is invalid")
	}
	if !dataSeen {
		return nil, 0, errors.New("WAV data chunk is missing")
	}
	if len(data)%pcm16BytesPerSample != 0 {
		return nil, 0, errors.New("WAV PCM16 data has an odd byte length")
	}
	return data, int(rate), nil
}

func readWAVChunk(content []byte, offset int) (string, []byte, int, error) {
	id := string(content[offset : offset+4])
	payloadOffset := offset + wavChunkHeaderSize
	rawSize := binary.LittleEndian.Uint32(content[offset+4 : offset+8])
	size := int(rawSize)
	if rawSize == wavUnknownSize {
		size = len(content) - payloadOffset
	}
	if size < 0 || payloadOffset+size > len(content) {
		return "", nil, 0, fmt.Errorf("WAV chunk %q exceeds input", id)
	}
	nextOffset := payloadOffset + size
	if size%2 != 0 {
		nextOffset++
	}
	return id, content[payloadOffset : payloadOffset+size], nextOffset, nil
}

// SilencePCM16 returns zero-valued little-endian PCM16 silence.
func SilencePCM16(rate, durationMS int) ([]byte, error) {
	if rate <= 0 {
		return nil, errors.New("sample rate must be positive")
	}
	if durationMS < 0 {
		return nil, errors.New("duration must not be negative")
	}
	samples := int64(rate) * int64(durationMS) / 1000
	if samples > int64(maxInt/pcm16BytesPerSample) {
		return nil, errors.New("silence duration is too large")
	}
	return make([]byte, int(samples)*pcm16BytesPerSample), nil
}

// ResamplePCM16 linearly resamples little-endian PCM16 audio.
func ResamplePCM16(pcm []byte, srcRate, dstRate int) ([]byte, error) {
	if srcRate <= 0 || dstRate <= 0 {
		return nil, errors.New("sample rates must be positive")
	}
	if len(pcm)%pcm16BytesPerSample != 0 {
		return nil, errors.New("PCM16 input has an odd byte length")
	}
	if len(pcm) == 0 {
		return nil, nil
	}
	if srcRate == dstRate {
		return append([]byte(nil), pcm...), nil
	}

	inputSamples := len(pcm) / pcm16BytesPerSample
	outputSamples := int(math.Round(float64(inputSamples) * float64(dstRate) / float64(srcRate)))
	if outputSamples < 1 {
		outputSamples = 1
	}
	output := make([]byte, outputSamples*pcm16BytesPerSample)
	if inputSamples == 1 || outputSamples == 1 {
		copy(output[:pcm16BytesPerSample], pcm[:pcm16BytesPerSample])
		return output, nil
	}

	for outIndex := 0; outIndex < outputSamples; outIndex++ {
		position := float64(outIndex) * float64(inputSamples-1) / float64(outputSamples-1)
		left := int(math.Floor(position))
		right := left + 1
		if right >= inputSamples {
			right = inputSamples - 1
		}
		weight := position - float64(left)
		leftValue := float64(int16(binary.LittleEndian.Uint16(pcm[left*pcm16BytesPerSample:])))
		rightValue := float64(int16(binary.LittleEndian.Uint16(pcm[right*pcm16BytesPerSample:])))
		value := int(math.Round(leftValue + (rightValue-leftValue)*weight))
		if value < minPCM16 {
			value = minPCM16
		} else if value > maxPCM16 {
			value = maxPCM16
		}
		binary.LittleEndian.PutUint16(output[outIndex*pcm16BytesPerSample:], uint16(int16(value)))
	}
	return output, nil
}

// PCM16ToG711ULaw encodes little-endian PCM16 samples as G.711 mu-law bytes.
func PCM16ToG711ULaw(pcm []byte) ([]byte, error) {
	if len(pcm)%pcm16BytesPerSample != 0 {
		return nil, errors.New("PCM16 input has an odd byte length")
	}
	output := make([]byte, len(pcm)/pcm16BytesPerSample)
	for index := range output {
		sample := int(int16(binary.LittleEndian.Uint16(pcm[index*pcm16BytesPerSample:])))
		output[index] = pcm16SampleToULaw(sample)
	}
	return output, nil
}

// PrepareAudio resamples PCM16 input and encodes it for the OpenClaw Gateway.
func PrepareAudio(pcm []byte, srcRate int, encoding string, targetRate int) (PreparedAudio, error) {
	resampled, err := ResamplePCM16(pcm, srcRate, targetRate)
	if err != nil {
		return PreparedAudio{}, err
	}
	switch normalizedEncoding(encoding) {
	case audioEncodingPCM16:
		return PreparedAudio{Data: resampled, Rate: targetRate, BytesPerSample: pcm16BytesPerSample, Encoding: audioEncodingPCM16}, nil
	case audioEncodingG711ULaw:
		encoded, err := PCM16ToG711ULaw(resampled)
		if err != nil {
			return PreparedAudio{}, err
		}
		return PreparedAudio{Data: encoded, Rate: targetRate, BytesPerSample: ulawBytesPerSample, Encoding: audioEncodingG711ULaw}, nil
	default:
		return PreparedAudio{}, fmt.Errorf("unsupported Gateway audio encoding %q", encoding)
	}
}

func normalizedEncoding(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", audioEncodingPCM16, "pcm_s16le", "linear16":
		return audioEncodingPCM16
	case audioEncodingG711ULaw, "ulaw", "mulaw", "mu-law":
		return audioEncodingG711ULaw
	default:
		return normalized
	}
}

func pcm16SampleToULaw(sample int) byte {
	sign := 0
	if sample < 0 {
		sign = 0x80
		sample = -sample
	}
	if sample > ulawClip {
		sample = ulawClip
	}
	sample += ulawBias

	exponent := 7
	exponentMask := 0x4000
	for exponent > 0 && sample&exponentMask == 0 {
		exponent--
		exponentMask >>= 1
	}
	mantissa := (sample >> (exponent + 3)) & 0x0f
	return byte(^(sign | exponent<<4 | mantissa) & 0xff)
}
