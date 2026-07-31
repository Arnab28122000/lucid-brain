// Package migrations embeds the memory layer's schema so the service can apply
// it at startup in evaluation mode, and so a Helm job can apply the identical
// bytes in production. Keeping one copy avoids the classic split where the
// docker-compose schema and the production schema quietly diverge.
package migrations

import (
	"embed"
	"io/fs"
	"sort"
)

//go:embed *.sql
var files embed.FS

// Migration is one ordered SQL file.
type Migration struct {
	Name string
	SQL  string
}

// All returns the migrations in filename order (which is version order).
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make([]Migration, 0, len(names))
	for _, n := range names {
		b, err := files.ReadFile(n)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Name: n, SQL: string(b)})
	}
	return out, nil
}
