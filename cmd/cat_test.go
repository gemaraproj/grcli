// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gemaraproj/go-gemara/bundle"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/content/oci"
)

// TestCat_Source_SingleFile is the end-to-end happy path: publish a one-file
// bundle to a local layout, then `cat --source` it and confirm stdout is the
// artifact content byte-for-byte, with no bundle.json leaking in.
func TestCat_Source_SingleFile(t *testing.T) {
	workdir := isolatedWorkdir(t)
	input := writeTempFile(t, workdir, "policy.yaml", policyYAML)
	layout := filepath.Join(workdir, "layout")
	runRoot(t, "publish", "--dry-run", "-f", input, "--output", layout, "--license", "Apache-2.0")

	out := runRoot(t, "cat", "--source", layout, "--version", "1.0.0")
	require.Equal(t, policyYAML, out, "cat must emit the artifact content verbatim")
	require.NotContains(t, out, "bundle-version", "cat must not emit bundle.json content")
	require.NotContains(t, out, "provenance", "cat must not emit provenance")
}

// TestCat_CacheHit_Offline proves cat is served from the cache with no network,
// exactly like unpack — same bogus --url, pre-seeded entry.
func TestCat_CacheHit_Offline(t *testing.T) {
	c := tempCache(t)
	isolatedWorkdir(t)
	const url = "https://hub.invalid.test"
	seed := &bundle.Bundle{Source: bundle.File{Name: "controls.yaml", Data: []byte("id: from-cache\n")}}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", seed, io.Discard)

	out := runRoot(t, "cat", "--url", url, "--repository", "acme/controls", "--version", "1.0.0")
	require.Equal(t, "id: from-cache\n", out)
}

// TestCat_CleanHit_ContentOnStdoutStderrQuiet asserts, with SEPARATE stdout and
// stderr buffers, that a clean cache hit puts artifact content on stdout and
// nothing on stderr — the pipe-clean guarantee cat exists for.
func TestCat_CleanHit_ContentOnStdoutStderrQuiet(t *testing.T) {
	c := tempCache(t)
	isolatedWorkdir(t)
	const url = "https://hub.invalid.test"
	seed := &bundle.Bundle{Source: bundle.File{Name: "controls.yaml", Data: []byte("id: from-cache\n")}}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", seed, io.Discard)

	stdout, stderr, err := executeRootSplit("cat", "--url", url, "--repository", "acme/controls", "--version", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "id: from-cache\n", stdout, "content must be on stdout")
	require.Empty(t, stderr, "a clean hit must not write to stderr")
}

// TestCat_Diagnostics_GoToStderrNotStdout is the regression guard for the
// stdout/stderr split. A corrupted cache blob makes resolveBundle emit a
// re-fetch diagnostic (then fail offline). The diagnostic MUST appear on stderr
// and MUST NOT leak onto stdout. If cat's wiring ever routes diagnostics to
// stdout, this test fails.
func TestCat_Diagnostics_GoToStderrNotStdout(t *testing.T) {
	c := tempCache(t)
	isolatedWorkdir(t)
	const url = "https://hub.invalid.test"
	seed := &bundle.Bundle{Source: bundle.File{Name: "controls.yaml", Data: []byte("id: from-cache\n")}}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", seed, io.Discard)
	corruptOneCacheBlob(t, c.Root())

	stdout, stderr, err := executeRootSplit("cat", "--url", url, "--repository", "acme/controls", "--version", "1.0.0")
	require.Error(t, err, "corrupt entry forces a network re-fetch that must fail offline")
	require.Contains(t, stderr, "cache", "the corruption/re-fetch diagnostic must be on stderr")
	require.Empty(t, stdout, "no diagnostic (or content) may leak onto stdout")
}

// corruptOneCacheBlob finds a stored file blob under the cache root and
// overwrites it so its digest no longer matches meta.json, forcing a cache
// corruption error on the next Get.
func corruptOneCacheBlob(t *testing.T, root string) {
	t.Helper()
	found := false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if found || info.IsDir() {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "files" {
			if werr := os.WriteFile(path, []byte("tampered-bytes"), 0o644); werr != nil {
				return werr
			}
			found = true
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, found, "expected a cached file blob to corrupt")
}

// TestCat_PublishFileKeyDoesNotBleed guards the viper key collision: 'file' in
// project config (or GRCLI_FILE) is publish's input-file list and must NOT act
// as cat's --file member selector.
// TestCat_PublishFileKeyIgnored: a project ./.grcli.yaml is no longer read at
// all, so a publish-oriented `file:` key in it cannot bleed into
// cat's --file selection. cat streams the full bundle on stdout; the ignored
// project file earns a migration warning on stderr (kept off the stdout pipe).
func TestCat_PublishFileKeyIgnored(t *testing.T) {
	c := tempCache(t)
	isolatedWorkdir(t)
	require.NoError(t, os.WriteFile(projectConfigFile, []byte("file: policy.yaml\n"), 0o644))
	const url = "https://hub.invalid.test"
	seed := &bundle.Bundle{Source: bundle.File{Name: "controls.yaml", Data: []byte("id: from-cache\n")}}
	putBundle(c, hostOf(url), "acme", "controls", "1.0.0", seed, io.Discard)

	stdout, stderr, err := executeRootSplit("cat", "--url", url, "--repository", "acme/controls", "--version", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "id: from-cache\n", stdout, "a project .grcli.yaml file: key must not select a bundle member in cat")
	require.Contains(t, stderr, "ignoring config", "the ignored project config should earn a migration warning")
}

func TestNoteCatOmittedImports(t *testing.T) {
	var buf bytes.Buffer
	noteCatOmittedImports(&buf, 2)
	require.Contains(t, buf.String(), "2 import(s) not included")
	require.Contains(t, buf.String(), "grcli unpack")
}

// TestCat_ImportsNoteEndToEnd drives the REAL command path for the omitted-
// imports diagnostic: a --source layout whose bundle carries an Imports layer
// (packed with go-gemara directly — grcli publish never produces one) must cat
// only the artifact files on stdout and put the omission note on stderr. Guards
// the runCat call wiring, which the unit test above cannot.
func TestCat_ImportsNoteEndToEnd(t *testing.T) {
	workdir := isolatedWorkdir(t)
	layout := filepath.Join(workdir, "layout")
	store, err := oci.New(layout)
	require.NoError(t, err)
	b := &bundle.Bundle{
		Manifest: bundle.Manifest{BundleVersion: "1.0", GemaraVersion: "0.5.0"},
		Source:   bundle.File{Name: "controls.yaml", Type: "ControlCatalog", Data: []byte("id: acme\n")},
		Imports:  []bundle.File{{Name: "dep.yaml", Type: "ControlCatalog", Data: []byte("id: dep\n")}},
	}
	desc, err := bundle.Pack(context.Background(), store, b)
	require.NoError(t, err)
	require.NoError(t, store.Tag(context.Background(), desc, "1.0.0"))

	stdout, stderr, err := executeRootSplit("cat", "--source", layout, "--version", "1.0.0")
	require.NoError(t, err)
	require.Equal(t, "id: acme\n", stdout, "stdout must carry the artifact files only")
	require.Contains(t, stderr, "1 import(s) not included", "the omission note must land on stderr")
	require.NotContains(t, stdout, "id: dep", "import content must not leak onto stdout")
}

func TestCat_MissingVersion_Errors(t *testing.T) {
	workdir := isolatedWorkdir(t)
	_, err := runRootExpectErr(t, "cat", "--source", filepath.Join(workdir, "x"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "--version is required")
}

func TestCatBundle_SingleFileVerbatim(t *testing.T) {
	var buf bytes.Buffer
	b := &bundle.Bundle{Source: bundle.File{Name: "a.yaml", Data: []byte("id: acme")}} // no trailing newline
	require.NoError(t, catBundle(b, "", &buf))
	require.Equal(t, "id: acme", buf.String(), "single file must be byte-exact, no added newline")
}

func TestCatBundle_FileSelection(t *testing.T) {
	var buf bytes.Buffer
	b := &bundle.Bundle{Source: bundle.File{Name: "b.yaml", Data: []byte("id: b\n")}}
	require.NoError(t, catBundle(b, "b.yaml", &buf))
	require.Equal(t, "id: b\n", buf.String(), "--file selects the source, verbatim")
}

func TestCatBundle_FileSelectionUnknown(t *testing.T) {
	b := &bundle.Bundle{Source: bundle.File{Name: "a.yaml", Data: []byte("x")}}
	err := catBundle(b, "nope.yaml", io.Discard)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no file named")
	require.Contains(t, err.Error(), "a.yaml", "error should list the available files")
}

func TestCatBundle_EmptyBundleErrors(t *testing.T) {
	err := catBundle(&bundle.Bundle{}, "", io.Discard)
	require.Error(t, err)
}
