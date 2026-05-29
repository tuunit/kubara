package catalog

import (
	"fmt"
	"os"
	"text/tabwriter"

	internalcatalog "github.com/kubara-io/kubara/internal/catalog"
	"github.com/rs/zerolog/log"
)

func ListCatalogs() error {
	entries, err := internalcatalog.ListCachedCatalogs()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		log.Info().Msg("No cached catalogs found")
		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "NAME\tVERSION\tREFERENCE\tDIGEST")
	for _, entry := range entries {
		_, _ = fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\n",
			entry.CatalogName,
			entry.CatalogVersion,
			entry.Reference,
			entry.ManifestDigest,
		)
	}
	return writer.Flush()
}
