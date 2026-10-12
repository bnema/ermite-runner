// SPDX-License-Identifier: MIT

package host

import (
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

func durationpbOf(d time.Duration) *durationpb.Duration { return durationpb.New(d) }
