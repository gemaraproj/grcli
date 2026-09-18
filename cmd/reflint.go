// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"io"

	"github.com/gemaraproj/grcli/internal/refs"
)

// warnReferences prints one line per reference that looks like it was meant
// to name a grc.store artifact but will not resolve everywhere (refs.Lint).
// Shared by validate and publish. Never fails the command: the hub accepts
// these bodies, and a reference to an external standard is not ours to
// judge. A body refs.Scan cannot read is silently skipped — validate
// reports schema problems on its own, and publish has already loaded it.
func warnReferences(w io.Writer, prefix string, body []byte) {
	a, err := refs.Scan(body)
	if err != nil {
		return
	}
	for _, msg := range a.Lint() {
		fmt.Fprintf(w, "%s%s\n", prefix, msg)
	}
}
