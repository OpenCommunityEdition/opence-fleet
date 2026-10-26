package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
)

// synthetic maps: a cache file's header (its signatures, version, name,
// build and scenario type, as the fleet's library reads them) and nothing
// of a game
var syntheticMaps = map[string]int{"ui": 2, "bloodgulch": 1, "sidewinder": 1, "prisoner": 1, "chillout": 1,
	"hangemhigh": 1, "dangercanyon": 1, "a10": 0}

// Header is a synthetic cache file's bytes: 4 KB, its header's.
func Header(name string, kind int, version int) []byte {
	data := make([]byte, 4096)
	copy(data[0:4], "daeh")
	binary.LittleEndian.PutUint32(data[4:8], uint32(version))
	binary.LittleEndian.PutUint32(data[8:12], uint32(len(data)))
	copy(data[0x20:0x40], name)
	copy(data[0x40:0x60], "01.10.12.2276")
	binary.LittleEndian.PutUint16(data[0x60:0x62], uint16(kind))
	copy(data[0x7fc:0x800], "toof")
	return data
}

func writeMaps(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "maps"), 0o755); err != nil {
		return err
	}
	for name, kind := range syntheticMaps {
		if err := os.WriteFile(filepath.Join(dir, "maps", name+".map"), Header(name, kind, 5), 0o644); err != nil {
			return err
		}
	}
	return nil
}
