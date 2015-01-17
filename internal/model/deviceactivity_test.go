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
	"testing"

	"github.com/syncthing/protocol"
)

func TestDeviceActivity(t *testing.T) {
	n0 := protocol.DeviceID([32]byte{1, 2, 3, 4})
	n1 := protocol.DeviceID([32]byte{5, 6, 7, 8})
	n2 := protocol.DeviceID([32]byte{9, 10, 11, 12})
	devices := map[protocol.DeviceID]uint32{
		n0: 0,
		n1: 0,
		n2: 0,
	}
	na := newDeviceActivity()

	d1, _ := na.leastBusy(devices)
	na.using(d1)
	if lb, _ := na.leastBusy(devices); lb == d1 {
		t.Errorf("Least busy device should not be %v", d1)
	}

	d2, _ := na.leastBusy(devices)
	na.using(d2)
	if lb, _ := na.leastBusy(devices); lb == d1 || lb == d2 {
		t.Errorf("Least busy device should not be %v or %v", d1, d2)
	}

	na.done(d1)
	if lb, _ := na.leastBusy(devices); lb == d2 {
		t.Errorf("Least busy device should not be %v", d2)
	}
}
