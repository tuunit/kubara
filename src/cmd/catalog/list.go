package catalog

import (
	"context"

	internalcatalog "github.com/kubara-io/kubara/internal/cmd/catalog"

	"github.com/urfave/cli/v3"
)

func NewCatalogList() *cli.Command {
	return &cli.Command{
		Name:        "list",
		Usage:       "List cached local and OCI-backed catalogs",
		UsageText:   "kubara catalog list",
		Description: "Lists cached catalogs from the local OCI cache, including local packages and cached OCI references.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return internalcatalog.ListCatalogs()
		},
	}
}
