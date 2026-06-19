package integration_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeInspectFixture builds a .snapshot ZIP (stored entries, so compressed and
// uncompressed sizes equal the byte lengths) for the inspect command to read.
func writeInspectFixture(t *testing.T, path string, entries map[string]int) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		require.NoError(t, err)
		_, err = w.Write(bytes.Repeat([]byte("x"), entries[name]))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
}

func TestSnapshotInspectLocalFileWithoutDocker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.snapshot")
	writeInspectFixture(t, path, map[string]int{
		"api_states/000000000000/s3/us-east-1/store.state.avro":       100,
		"api_states/000000000000/dynamodb/us-east-1/store.state.avro": 50,
		"assets/ecr/layer1": 4000,
		"assets/s3/obj1":    1000,
	})

	stdout, stderr, err := runLstk(t, testContext(t), dir,
		testEnvWithHome(t.TempDir(), ""),
		"--non-interactive", "snapshot", "inspect", path,
	)
	require.NoError(t, err, "lstk snapshot inspect failed: %s", stderr)

	// Groups are friendly-named and sorted largest-first, and asset services
	// roll up into semantic categories (ecr -> Container Images).
	assert.Contains(t, stdout, "Data Assets")
	assert.Contains(t, stdout, "Container Images")
	assert.Contains(t, stdout, "Control Plane")
	assert.Less(t, strings.Index(stdout, "Data Assets"), strings.Index(stdout, "Control Plane"),
		"Data Assets (larger) should appear before Control Plane")
	assert.Contains(t, stdout, "TOTAL")
	assert.Contains(t, stdout, "100%")
}

func TestSnapshotInspectJSONWithoutDocker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.snapshot")
	writeInspectFixture(t, path, map[string]int{
		"assets/ecr/layer1": 4000,
		"api_states/000000000000/s3/us-east-1/store.state.avro": 100,
	})

	stdout, stderr, err := runLstk(t, testContext(t), dir,
		testEnvWithHome(t.TempDir(), ""),
		"--non-interactive", "snapshot", "inspect", path, "--json",
	)
	require.NoError(t, err, "lstk snapshot inspect --json failed: %s", stderr)

	var got struct {
		Path              string `json:"path"`
		TotalUncompressed int64  `json:"total_uncompressed_bytes"`
		Groups            []struct {
			Label        string `json:"label"`
			Uncompressed int64  `json:"uncompressed_bytes"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout was not valid JSON:\n%s", stdout)
	assert.Equal(t, int64(4100), got.TotalUncompressed)
	require.NotEmpty(t, got.Groups)
	assert.Equal(t, "Data Assets", got.Groups[0].Label)
}

func TestSnapshotInspectRejectsPodRef(t *testing.T) {
	t.Parallel()

	_, stderr, err := runLstk(t, testContext(t), t.TempDir(),
		testEnvWithHome(t.TempDir(), ""),
		"--non-interactive", "snapshot", "inspect", "pod:my-baseline",
	)
	requireExitCode(t, 1, err)
	assert.Contains(t, strings.ToLower(stderr), "local")
	assert.Contains(t, stderr, "snapshot show")
}

func TestSnapshotInspectInvalidFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.snapshot")
	require.NoError(t, os.WriteFile(path, []byte("not a zip"), 0o600))

	stdout, _, err := runLstk(t, testContext(t), dir,
		testEnvWithHome(t.TempDir(), ""),
		"--non-interactive", "snapshot", "inspect", path,
	)
	requireExitCode(t, 1, err)
	assert.Contains(t, stdout, "not a valid snapshot archive")
}
