// SPDX-License-Identifier: Apache-2.0

package auth

import "time"

// SetClock overrides a Store's clock so STS session expiry can be driven
// deterministically from tests without sleeping. Test-only.
func SetClock(s *Store, now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}
