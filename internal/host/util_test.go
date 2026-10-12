// SPDX-License-Identifier: MIT

package host

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

func durationpbOf(d time.Duration) *durationpb.Duration { return durationpb.New(d) }
