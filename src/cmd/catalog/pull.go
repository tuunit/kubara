package catalog

import (
	"context"

	internalcatalog "github.com/kubara-io/kubara/internal/cmd/catalog"

	"github.com/urfave/cli/v3"
)

type catalogPullFlags struct {
	force    bool
	insecure bool
}

func NewCatalogPull() *cli.Command {
	flags := &catalogPullFlags{}
	return &cli.Command{
		Name:        "pull",
		Usage:       "Pull a catalog OCI artifact into the local cache",
		UsageText:   "kubara catalog pull [--force] [--insecure] oci://registry/repository:x.y.z",
		Description: "Pulls a catalog OCI artifact into the local cache, using the cache when possible unless --force is set. Use --insecure to ignore TLS certificate verification issues for the registry.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "force",
				Usage:       "Refresh an already cached tagged catalog reference.",
				Destination: &flags.force,
			},
			&cli.BoolFlag{
				Name:        "insecure",
				Usage:       "Ignore TLS certificate verification issues for the registry connection.",
				Destination: &flags.insecure,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 1 {
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
			return internalcatalog.PullCatalog(cmd.Args().First(), registryConfigPath, flags.force, flags.insecure)
		},
	}
}
