package catalog

import (
	"context"

	internalcatalog "github.com/kubara-io/kubara/internal/cmd/catalog"

	"github.com/urfave/cli/v3"
)

type catalogPushFlags struct {
	from     string
	insecure bool
}

func NewCatalogPush() *cli.Command {
	flags := &catalogPushFlags{}
	return &cli.Command{
		Name:        "push",
		Usage:       "Package the current catalog or push an existing cached catalog to an OCI registry",
		UsageText:   "kubara catalog push [--from oci://source/repository:x.y.z] [--insecure] oci://target/repository:x.y.z",
		Description: "Packages the current catalog directory and pushes it to an OCI registry, or with --from copies an existing cached or resolvable OCI catalog reference to a new OCI reference. Use --insecure to ignore TLS certificate verification issues for registry connections.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "from",
				Usage:       "Push an existing cached or resolvable OCI catalog reference instead of packaging the current directory.",
				Destination: &flags.from,
			},
			&cli.BoolFlag{
				Name:        "insecure",
				Usage:       "Ignore TLS certificate verification issues for registry connections.",
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
			return internalcatalog.PushCatalog(cwd, flags.from, cmd.Args().First(), registryConfigPath, flags.insecure)
		},
	}
}
