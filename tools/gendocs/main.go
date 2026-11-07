// Command gendocs rewrites docs/ from the provider's schemas and the examples (see internal/docgen), and leaves the guides
// in docs/guides that a person wrote alone.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gitlab.virtscale.nl/virtscale/virtscale-platform/terraform-provider-virtscale/internal/docgen"
)

func main() {
	pages, err := docgen.Generate(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
	// Only what is generated is cleared: docs/guides also holds the guides a person writes.
	for _, dir := range []string{"docs/resources", "docs/data-sources"} {
		_ = os.RemoveAll(dir)
	}
	for name, content := range pages {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "gendocs:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "gendocs:", err)
			os.Exit(1)
		}
	}
}
