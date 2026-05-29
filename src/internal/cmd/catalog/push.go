package catalog

import (
	internalcatalog "github.com/kubara-io/kubara/internal/catalog"
	"github.com/rs/zerolog/log"
)

func PushCatalog(catalogRoot, sourceReference, reference, registryConfigPath string, insecure bool) error {
	result, err := internalcatalog.PushCatalog(internalcatalog.PushOptions{
		CatalogRoot:        catalogRoot,
		SourceReference:    sourceReference,
		Reference:          reference,
		RegistryConfigPath: registryConfigPath,
		Insecure:           insecure,
	})
	if err != nil {
		return err
	}

	log.Info().Msgf(
		"Catalog %q version %q has been pushed to %s",
		result.Manifest.Metadata.Name,
		result.Manifest.Spec.Version,
		reference,
	)
	return nil
}
