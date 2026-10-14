// SPDX-License-Identifier: MIT

package executor

import (
	"strconv"
	"strings"
)

// The start-action and end-action markers the runner prints around each step of
// a composite action (CompositeActionHandler.RunStepsAsync, :323 and :415-466).
// GitHub's runner emits them when the EmitCompositeMarkers feature is on, which
// is the case for the recorded runs, so they are always printed here.

// maxMarkerDisplayName is CompositeActionHandler.MaxDisplayNameLength.
const maxMarkerDisplayName = 1000

// markerEscaper is ActionCommand.EscapeValue (ActionCommand.cs:211-225): the
// escape mappings applied last to first, so that "%" goes first and nothing is
// escaped twice.
var markerEscaper = strings.NewReplacer("%", "%25", "]", "%5D", "\n", "%0A", "\r", "%0D", ";", "%3B")

// sanitizeDisplayName is CompositeActionHandler.SanitizeDisplayName (:552): the
// first line without its leading white space, cut to 1000 UTF-16 code units.
func sanitizeDisplayName(name string) string {
	r := firstLine(name)
	units := 0
	for i, c := range r {
		w := 1
		if c >= 0x10000 {
			w = 2
		}
		if units+w > maxMarkerDisplayName {
			return r[:i]
		}
		units += w
	}
	return r
}

func startMarker(display, id string) string {
	return "##[start-action display=" + markerEscaper.Replace(sanitizeDisplayName(display)) + ";id=" + markerEscaper.Replace(id) + "]"
}

func endMarker(id, outcome, conclusion string, ms int64) string {
	return "##[end-action id=" + markerEscaper.Replace(id) + ";outcome=" + outcome + ";conclusion=" + conclusion + ";duration_ms=" + strconv.FormatInt(ms, 10) + "]"
}

// markerResult is TaskResult.ToActionResult in lower case.
func markerResult(r stepResult) string {
	switch r {
	case resFailed:
		return "failure"
	case resCanceled:
		return "cancelled"
	case resSkipped:
		return "skipped"
	}
	return "success"
}
