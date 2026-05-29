package catalog

import (
	"context"
	"fmt"

	internalcatalog "github.com/kubara-io/kubara/internal/cmd/catalog"
	"github.com/kubara-io/kubara/internal/utils"

	"github.com/urfave/cli/v3"
)

func NewCatalogUnpackage() *cli.Command {
	return &cli.Command{
		Name:        "unpackage",
		Usage:       "Materialize a cached OCI catalog as an editable directory",
		UsageText:   "kubara catalog unpackage oci://registry/repository:x.y.z [directory]",
		Description: "Copies a cached OCI catalog into an editable directory, pulling it first when it is not already cached.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() < 1 || cmd.Args().Len() > 2 {
				cli.ShowSubcommandHelpAndExit(cmd, 1)
			}

			cwd, err := resolveCatalogCommandWorkingDir(cmd)
			if err != nil {
				return err
			}
			registryConfigPath, err := resolveRegistryConfigPath(cmd, cwd)
			if err != nil {
				return err
			}

			outputPath := ""
			if cmd.Args().Len() == 2 {
				outputPath, err = utils.GetFullPath(cmd.Args().Get(1), cwd)
				if err != nil {
					return fmt.Errorf("get output directory: %w", err)
				}
			}

			return internalcatalog.UnpackageCatalog(cmd.Args().First(), outputPath, cwd, registryConfigPath)
		},
	}
}
