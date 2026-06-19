package snapshot_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/localstack/lstk/internal/snapshot"
)

// writeSnapshot builds a .snapshot ZIP whose entries are stored (uncompressed)
// so each entry's compressed and uncompressed sizes equal its byte length.
func writeSnapshot(t *testing.T, entries map[string]int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.snapshot")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatalf("create header %q: %v", name, err)
		}
		if _, err := w.Write(bytes.Repeat([]byte("x"), entries[name])); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return path
}

func TestComputeInspect(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, map[string]int{
		"api_states/": 0, // directory entry, must be skipped
		// api_states/<account>/<service>/<region>/...: the service is the 3rd
		// segment and is aggregated across accounts and regions.
		"api_states/000000000000/s3/us-east-1/store.state.avro":       100,
		"api_states/949334387222/s3/us-east-1/store.state.avro":       30, // 2nd account, same service
		"api_states/000000000000/dynamodb/us-east-1/store.state.avro": 50,
		// assets/<service>/...: services roll up into semantic categories.
		"assets/ecr/layer1":                         1000, // Container Images
		"assets/rds/dump.sql":                       400,  // Databases
		"assets/dynamodb/000000000000_us-east-1.db": 200,  // Databases
		"manifest.json":                             10,   // root-level file
	})

	ev, err := snapshot.ComputeInspect(path)
	if err != nil {
		t.Fatalf("ComputeInspect: %v", err)
	}

	if got, want := ev.TotalUncompressed, int64(1790); got != want {
		t.Fatalf("TotalUncompressed = %d, want %d", got, want)
	}
	if got, want := ev.TotalCompressed, int64(1790); got != want {
		t.Fatalf("TotalCompressed = %d, want %d (stored entries)", got, want)
	}

	if len(ev.Groups) != 3 {
		t.Fatalf("expected 3 groups, got %d: %+v", len(ev.Groups), ev.Groups)
	}

	// Groups sorted largest-first: Data Assets (1600) > Control Plane (180) > (root) (10).
	if ev.Groups[0].Label != "Data Assets" || ev.Groups[0].Uncompressed != 1600 {
		t.Fatalf("group[0] = %+v, want Data Assets 1600", ev.Groups[0])
	}
	if ev.Groups[1].Label != "Control Plane" || ev.Groups[1].Uncompressed != 180 {
		t.Fatalf("group[1] = %+v, want Control Plane 180", ev.Groups[1])
	}
	if ev.Groups[2].Label != "(root)" || ev.Groups[2].Uncompressed != 10 {
		t.Fatalf("group[2] = %+v, want (root) 10", ev.Groups[2])
	}

	// Data Assets children are CATEGORIES, largest-first: Container Images (ecr,
	// 1000) > Databases (rds 400 + dynamodb 200 = 600).
	da := ev.Groups[0]
	if len(da.Children) != 2 {
		t.Fatalf("Data Assets categories = %+v, want 2 (Container Images, Databases)", da.Children)
	}
	if da.Children[0].Label != "Container Images" || da.Children[0].Uncompressed != 1000 {
		t.Fatalf("Data Assets category[0] = %+v, want Container Images 1000", da.Children[0])
	}
	if len(da.Children[0].Children) != 1 || da.Children[0].Children[0].Label != "ecr" {
		t.Fatalf("Container Images services = %+v, want [ecr]", da.Children[0].Children)
	}
	dbs := da.Children[1]
	if dbs.Label != "Databases" || dbs.Uncompressed != 600 {
		t.Fatalf("Data Assets category[1] = %+v, want Databases 600", dbs)
	}
	// Databases aggregates two services, sub-sorted: rds (400) > dynamodb (200).
	if len(dbs.Children) != 2 || dbs.Children[0].Label != "rds" || dbs.Children[0].Uncompressed != 400 {
		t.Fatalf("Databases services = %+v, want rds first", dbs.Children)
	}
	if dbs.Children[1].Label != "dynamodb" || dbs.Children[1].Uncompressed != 200 {
		t.Fatalf("Databases service[1] = %+v, want dynamodb 200", dbs.Children[1])
	}

	// Control Plane children are SERVICES (not account ids), with s3 aggregated
	// across both accounts: s3 (100+30=130) > dynamodb (50).
	cp := ev.Groups[1]
	if len(cp.Children) != 2 {
		t.Fatalf("Control Plane children = %+v, want 2 (s3, dynamodb)", cp.Children)
	}
	if cp.Children[0].Label != "s3" || cp.Children[0].Uncompressed != 130 {
		t.Fatalf("Control Plane child[0] = %+v, want s3 130 (aggregated across accounts)", cp.Children[0])
	}
	if cp.Children[1].Label != "dynamodb" || cp.Children[1].Uncompressed != 50 {
		t.Fatalf("Control Plane child[1] = %+v, want dynamodb 50", cp.Children[1])
	}
	for _, c := range cp.Children {
		if c.Label == "000000000000" || c.Label == "949334387222" {
			t.Fatalf("Control Plane child %q is an account id; services must be aggregated across accounts", c.Label)
		}
	}
}

func TestComputeInspectInvalidFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "not-a-zip.snapshot")
	if err := os.WriteFile(path, []byte("definitely not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.ComputeInspect(path); err == nil {
		t.Fatal("expected error for a non-zip file, got nil")
	}
}

func TestInspectJSON(t *testing.T) {
	t.Parallel()

	path := writeSnapshot(t, map[string]int{
		"assets/ecr/layer1": 1000,
		"api_states/000000000000/s3/us-east-1/store.state.avro": 100,
	})

	var buf bytes.Buffer
	if err := snapshot.InspectJSON(path, &buf); err != nil {
		t.Fatalf("InspectJSON: %v", err)
	}

	var got struct {
		TotalUncompressed int64 `json:"total_uncompressed_bytes"`
		Groups            []struct {
			Label        string `json:"label"`
			Uncompressed int64  `json:"uncompressed_bytes"`
			Children     []struct {
				Label        string `json:"label"`
				Uncompressed int64  `json:"uncompressed_bytes"`
			} `json:"children"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal JSON: %v\n%s", err, buf.String())
	}
	if got.TotalUncompressed != 1100 {
		t.Fatalf("total = %d, want 1100", got.TotalUncompressed)
	}
	if len(got.Groups) == 0 || got.Groups[0].Label != "Data Assets" {
		t.Fatalf("groups = %+v, want Data Assets first", got.Groups)
	}
}

func TestParseInspectable(t *testing.T) {
	t.Parallel()

	t.Run("rejects pod refs pointing at show", func(t *testing.T) {
		t.Parallel()
		if _, err := snapshot.ParseInspectable("pod:my-baseline", ""); err == nil {
			t.Fatal("expected error for pod: ref")
		}
	})

	t.Run("rejects remote schemes", func(t *testing.T) {
		t.Parallel()
		if _, err := snapshot.ParseInspectable("s3://bucket/key", ""); err == nil {
			t.Fatal("expected error for s3:// ref")
		}
	})

	t.Run("accepts an existing local file", func(t *testing.T) {
		t.Parallel()
		path := writeSnapshot(t, map[string]int{"assets/s3/obj1": 1})
		dest, err := snapshot.ParseInspectable(path, "")
		if err != nil {
			t.Fatalf("ParseInspectable: %v", err)
		}
		if dest.Kind != snapshot.KindLocal || dest.Value != path {
			t.Fatalf("dest = %+v, want KindLocal %q", dest, path)
		}
	})

	t.Run("errors on a missing local file", func(t *testing.T) {
		t.Parallel()
		if _, err := snapshot.ParseInspectable(filepath.Join(t.TempDir(), "nope.snapshot"), ""); err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}
