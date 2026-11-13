package apptainer

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

type testSIFPart struct {
	dataType int32
	name     string
	content  string
}

// testSIF lays out a SIF as Apptainer builds one from an OCI image: a
// definition file and, unless config is empty, the image's configuration.
func testSIF(config string) []byte {
	parts := []testSIFPart{{dataType: 0x4001, content: "bootstrap: docker\n"}}
	if config != "" {
		parts = append(parts, testSIFPart{dataType: sifGenericJSON, name: sifOCIConfigName, content: config})
	}
	header := sifHeader{DescriptorsTotal: int64(len(parts)), DescriptorsOffset: int64(binary.Size(sifHeader{}))}
	copy(header.Magic[:], sifMagic)
	header.DescriptorsSize = header.DescriptorsTotal * int64(binary.Size(sifDescriptor{}))
	header.DataOffset = header.DescriptorsOffset + header.DescriptorsSize
	var data bytes.Buffer
	descriptors := make([]sifDescriptor, len(parts))
	for index, part := range parts {
		descriptors[index] = sifDescriptor{DataType: part.dataType, Used: true, ID: uint32(index + 1), Offset: header.DataOffset + int64(data.Len()), Size: int64(len(part.content))}
		copy(descriptors[index].Name[:], part.name)
		data.WriteString(part.content)
	}
	header.DataSize = int64(data.Len())
	var file bytes.Buffer
	_ = binary.Write(&file, binary.LittleEndian, header)
	_ = binary.Write(&file, binary.LittleEndian, descriptors)
	file.Write(data.Bytes())
	return file.Bytes()
}

func TestSIFWorkdirReadsTheImageConfiguration(t *testing.T) {
	directory := t.TempDir()
	for name, c := range map[string]struct {
		content []byte
		want    string
		ok      bool
	}{
		"workdir":       {testSIF(`{"Env":["A=b"],"WorkingDir":"/app/"}`), "/app", true},
		"no workdir":    {testSIF(`{"Env":["A=b"]}`), "/", true},
		"no config":     {testSIF(""), "/", true},
		"relative":      {testSIF(`{"WorkingDir":"app"}`), "", false},
		"invalid json":  {testSIF(`{"WorkingDir":`), "", false},
		"not a SIF":     {[]byte("not a SIF file"), "", false},
		"truncated SIF": {testSIF(`{"WorkingDir":"/app"}`)[:200], "", false},
	} {
		file := filepath.Join(directory, name+".sif")
		if err := os.WriteFile(file, c.content, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := sifWorkdir(file); got != c.want || (err == nil) != c.ok {
			t.Errorf("%s: sifWorkdir = %q, %v", name, got, err)
		}
	}
	if _, err := sifWorkdir(filepath.Join(directory, "missing.sif")); err == nil {
		t.Error("read a missing SIF")
	}
}
