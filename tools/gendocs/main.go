// Command gendocs rewrites docs/ from the provider's schemas and the examples (see internal/docgen).
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
	_ = os.RemoveAll("docs")
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
