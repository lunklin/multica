//go:build attachmentadminintegration && (darwin || linux)

package dbintegration

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
)

// Pinned official migration bytes; no schema is applied by this harness.
const schemaSHA = "aa2b3501a7f349b33abd74a24939541d3f582082f83476b21d5b663cb07c4822"

//go:embed schema-source.json
var schemaSource []byte

func expectedVersions() []string {
	var source struct {
		Files map[string]string `json:"files"`
	}
	if json.Unmarshal(schemaSource, &source) != nil {
		return nil
	}
	versions := make([]string, 0, len(source.Files))
	for name := range source.Files {
		versions = append(versions, strings.TrimSuffix(name, ".up.sql"))
	}
	sort.Strings(versions)
	return versions
}
