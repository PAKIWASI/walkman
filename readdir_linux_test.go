package walkman

import (
	"fmt"
	"testing"
)

func Test_readDirRaw(t *testing.T) {

	buf := make([]byte, 256)

	names := make([]string, 10)
	dTypes := make([]uint8, 10)
	inos := make([]uint64, 10)

	onEntry := func(name []byte, dType uint8, ino uint64) error {
		names = append(names, string(name))
		dTypes = append(dTypes, dType)
		inos = append(inos, ino)
		return nil
	}

	tests := []struct {
		name string
		dirPath string
		wantErr bool
	}{
		{"test", "/home/wasi/Downloads", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotErr := readDirRaw(tt.dirPath, buf, onEntry)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("readDirRaw() failed: %v", gotErr)
				}
				return
			}
			if tt.wantErr {
				t.Fatal("readDirRaw() succeeded unexpectedly")
			}

			fmt.Println(names)
			fmt.Println(dTypes)
			fmt.Println(inos)
		})
	}
}

