// SPDX-License-Identifier: Apache-2.0

package lock

import "time"

// SetClock overrides a LocalLocker's clock so lease expiry can be driven
// deterministically from tests without sleeping. Test-only.
func SetClock(l *LocalLocker, now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}
