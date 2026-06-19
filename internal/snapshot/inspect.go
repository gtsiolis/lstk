package snapshot

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/localstack/lstk/internal/output"
)

// friendlyGroupNames maps the well-known top-level directories of a .snapshot
// archive to human labels. Any other top-level path is reported under its raw
// name; its bytes are never hidden or misattributed.
var friendlyGroupNames = map[string]string{
	"api_states": "Control Plane",
	"assets":     "Data Assets",
}

// assetCategories groups the per-service directories under assets/ into the
// human categories shown for Data Assets. Unknown services fall into
// "Other Assets" so nothing is dropped.
var assetCategories = map[string]string{
	"s3":          "S3 Objects",
	"ecr":         "Container Images",
	"dynamodb":    "Databases",
	"rds":         "Databases",
	"elasticache": "Search & Cache",
	"opensearch":  "Search & Cache",
	"cloudwatch":  "Other Assets",
	"kinesis":     "Other Assets",
}

// ComputeInspect opens a local .snapshot archive (a ZIP) and tallies its byte
// usage into a size tree, with no running emulator, platform call, or auth.
// Each node's children are sorted largest-first by uncompressed size.
func ComputeInspect(path string) (output.SnapshotInspectedEvent, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return output.SnapshotInspectedEvent{}, err
	}
	defer func() { _ = r.Close() }()

	root := newSizeNode("")
	for _, f := range r.File {
		if strings.HasSuffix(f.Name, "/") {
			continue // directory entry, no payload
		}
		root.add(classifyPath(f.Name), int64(f.UncompressedSize64), int64(f.CompressedSize64))
	}

	return output.SnapshotInspectedEvent{
		Path:              path,
		TotalUncompressed: root.unc,
		TotalCompressed:   root.comp,
		Groups:            root.toOutput(),
	}, nil
}

// Inspect computes a local snapshot's size breakdown and emits it as a
// SnapshotInspectedEvent. On a read/parse error it emits a friendly ErrorEvent
// and returns a silent error.
func Inspect(path string, sink output.Sink) error {
	ev, err := ComputeInspect(path)
	if err != nil {
		sink.Emit(output.ErrorEvent{
			Title:   fmt.Sprintf("Could not read snapshot %q", path),
			Summary: "The file is not a valid snapshot archive.",
			Actions: []output.ErrorAction{
				{Label: "Inspect a saved snapshot:", Value: "lstk snapshot inspect ./my-snapshot.snapshot"},
			},
		})
		return output.NewSilentError(fmt.Errorf("inspect snapshot %q: %w", path, err))
	}
	sink.Emit(output.DeferredEvent{Inner: ev})
	return nil
}

// InspectJSON writes a local snapshot's size breakdown to w as JSON. This is a
// machine-output mode used by `snapshot inspect --json`; sizes are raw bytes so
// callers (scripts, agents) can compute their own percentages.
func InspectJSON(path string, w io.Writer) error {
	ev, err := ComputeInspect(path)
	if err != nil {
		return fmt.Errorf("inspect snapshot %q: %w", path, err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(ev)
}

// classifyPath maps an archive entry path to its label path from the top-level
// group down to a service. The two known layouts place the service at different
// depths:
//
//	api_states/<account>/<service>/[<region>/]...  -> [Control Plane, service]
//	    (aggregated across accounts and regions)
//	assets/<service>/...                           -> [Data Assets, category, service]
//
// A file at the archive root becomes [(root), filename]; any other layout falls
// back to [group, second-segment].
func classifyPath(name string) []string {
	name = strings.TrimPrefix(name, "./")
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return []string{"(root)", parts[0]}
	}
	switch parts[0] {
	case "api_states":
		if len(parts) >= 3 && parts[2] != "" {
			return []string{friendlyGroupNames["api_states"], parts[2]}
		}
	case "assets":
		if parts[1] != "" {
			return []string{friendlyGroupNames["assets"], assetCategory(parts[1]), parts[1]}
		}
	}
	return []string{groupLabel(parts[0]), parts[1]}
}

func assetCategory(service string) string {
	if c, ok := assetCategories[service]; ok {
		return c
	}
	return "Other Assets"
}

func groupLabel(top string) string {
	if top == "" {
		return "(root)"
	}
	if friendly, ok := friendlyGroupNames[top]; ok {
		return friendly
	}
	return top
}

// sizeNode accumulates byte usage while building the size tree.
type sizeNode struct {
	label     string
	unc, comp int64
	children  map[string]*sizeNode
	order     []string
}

func newSizeNode(label string) *sizeNode {
	return &sizeNode{label: label, children: map[string]*sizeNode{}}
}

// add tallies one entry's sizes into the root and every node along path.
func (n *sizeNode) add(path []string, unc, comp int64) {
	n.unc += unc
	n.comp += comp
	cur := n
	for _, label := range path {
		child := cur.children[label]
		if child == nil {
			child = newSizeNode(label)
			cur.children[label] = child
			cur.order = append(cur.order, label)
		}
		child.unc += unc
		child.comp += comp
		cur = child
	}
}

// toOutput converts the node's children into output nodes, sorted largest-first.
func (n *sizeNode) toOutput() []output.SnapshotSizeNode {
	out := make([]output.SnapshotSizeNode, 0, len(n.order))
	for _, label := range n.order {
		c := n.children[label]
		out = append(out, output.SnapshotSizeNode{
			Label:        c.label,
			Uncompressed: c.unc,
			Compressed:   c.comp,
			Children:     c.toOutput(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Uncompressed != out[j].Uncompressed {
			return out[i].Uncompressed > out[j].Uncompressed
		}
		return out[i].Label < out[j].Label
	})
	return out
}
