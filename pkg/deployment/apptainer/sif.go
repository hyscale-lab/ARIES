package apptainer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// A SIF is a fixed header followed by a table of fixed-size descriptors, all
// little-endian, as the format's reference implementation
// (github.com/apptainer/sif) reads them. A SIF that Apptainer builds from an
// OCI image keeps the image's configuration in a generic JSON descriptor.
const (
	sifMagic           = "SIF_MAGIC\x00"
	sifGenericJSON     = 0x4006
	sifOCIConfigName   = "oci-config.json"
	maxSIFDescriptors  = 1 << 10
	maxSIFConfigLength = 1 << 20
)

type sifHeader struct {
	LaunchScript      [32]byte
	Magic             [10]byte
	Version           [3]byte
	Arch              [3]byte
	ID                [16]byte
	CreatedAt         int64
	ModifiedAt        int64
	DescriptorsFree   int64
	DescriptorsTotal  int64
	DescriptorsOffset int64
	DescriptorsSize   int64
	DataOffset        int64
	DataSize          int64
}

type sifDescriptor struct {
	DataType        int32
	Used            bool
	ID              uint32
	GroupID         uint32
	LinkedID        uint32
	Offset          int64
	Size            int64
	SizeWithPadding int64
	CreatedAt       int64
	ModifiedAt      int64
	UID             int64
	GID             int64
	Name            [128]byte
	Extra           [384]byte
}

// sifWorkdir returns the working directory of the OCI image a SIF was built
// from. A SIF without an OCI configuration, or an image without a WORKDIR,
// works in the root, as a Docker container of that image would.
func sifWorkdir(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var header sifHeader
	if err := binary.Read(f, binary.LittleEndian, &header); err != nil {
		return "", fmt.Errorf("read SIF header: %w", err)
	}
	if string(header.Magic[:]) != sifMagic {
		return "", errors.New("not a SIF file")
	}
	if header.DescriptorsTotal < 0 || header.DescriptorsTotal > maxSIFDescriptors {
		return "", errors.New("SIF descriptor count is out of range")
	}
	descriptors := io.NewSectionReader(f, header.DescriptorsOffset, header.DescriptorsTotal*int64(binary.Size(sifDescriptor{})))
	for range header.DescriptorsTotal {
		var descriptor sifDescriptor
		if err := binary.Read(descriptors, binary.LittleEndian, &descriptor); err != nil {
			return "", fmt.Errorf("read SIF descriptor: %w", err)
		}
		if !descriptor.Used || descriptor.DataType != sifGenericJSON || string(bytes.TrimRight(descriptor.Name[:], "\x00")) != sifOCIConfigName {
			continue
		}
		if descriptor.Size < 0 || descriptor.Size > maxSIFConfigLength {
			return "", errors.New("SIF OCI configuration size is out of range")
		}
		content := make([]byte, descriptor.Size)
		if _, err := f.ReadAt(content, descriptor.Offset); err != nil {
			return "", fmt.Errorf("read SIF OCI configuration: %w", err)
		}
		var config struct{ WorkingDir string }
		if err := json.Unmarshal(content, &config); err != nil {
			return "", fmt.Errorf("parse SIF OCI configuration: %w", err)
		}
		if config.WorkingDir == "" {
			return "/", nil
		}
		if !path.IsAbs(config.WorkingDir) || strings.ContainsRune(config.WorkingDir, 0) {
			return "", fmt.Errorf("image working directory %q is not absolute", config.WorkingDir)
		}
		return path.Clean(config.WorkingDir), nil
	}
	return "/", nil
}
