// Package drivers links every terminal driver into the binary. Import it
// for its side effect; add a vendor here when its driver lands.
package drivers

import (
	_ "github.com/UPAX-platform/attendance-relay/internal/device/zkteco"
)
