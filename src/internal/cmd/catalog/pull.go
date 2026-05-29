package catalog

import (
	internalcatalog "github.com/kubara-io/kubara/internal/catalog"
	"github.com/rs/zerolog/log"
)

func PullCatalog(reference, registryConfigPath string, force bool, insecure bool) error {
	artifact, err := internalcatalog.EnsureRemoteCatalogCached(internalcatalog.PullOptions{
		Reference:          reference,
		RegistryConfigPath: registryConfigPath,
		Force:              force,
		Insecure:           insecure,
	})
	if err != nil {
		return err
	}

	log.Info().Msgf(
		"Catalog %q version %q is cached with digest %s",
		artifact.CatalogName,
		artifact.CatalogVersion,
		artifact.ManifestDigest,
	)
	return nil
}
