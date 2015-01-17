// Copyright (C) 2014 The Syncthing Authors.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
// more details.
//
// You should have received a copy of the GNU General Public License along
// with this program. If not, see <http://www.gnu.org/licenses/>.

package model

import (
	"path/filepath"
	"sync"

	"github.com/syncthing/protocol"
)

// Helper struct which helps managing temporary indexes
type tempIndex struct {
	// device -> folder+file -> block hash -> struct{}
	indexes map[protocol.DeviceID]map[string]map[string]struct{}
	mut     sync.RWMutex
}

func newTempIndex() *tempIndex {
	return &tempIndex{
		indexes: make(map[protocol.DeviceID]map[string]map[string]struct{}),
	}
}

// Update temporary index for a given device and given folder
// Replaces the existing temporary index.
func (i *tempIndex) Update(device protocol.DeviceID, folder string, files []protocol.FileInfo) {
	i.mut.Lock()
	index := make(map[string]map[string]struct{})
	for _, file := range files {
		blocks := make(map[string]struct{}, len(file.Blocks))
		for _, block := range file.Blocks {
			blocks[string(block.Hash)] = struct{}{}
		}
		index[filepath.Join(folder, file.Name)] = blocks
	}
	i.indexes[device] = index
	i.mut.Unlock()
}

// Looks up which devices have the given block in the given file.
func (i *tempIndex) Lookup(folder, file string, hash []byte) []protocol.DeviceID {
	i.mut.RLock()
	devices := make([]protocol.DeviceID, 0, len(i.indexes))
	for device, index := range i.indexes {
		if blocks, ok := index[filepath.Join(folder, file)]; ok {
			if _, ok := blocks[string(hash)]; ok {
				devices = append(devices, device)
			}
		}
	}
	i.mut.RUnlock()
	return devices
}
