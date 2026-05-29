package catalog

import (
	internalcatalog "github.com/kubara-io/kubara/internal/catalog"
	"github.com/rs/zerolog/log"
)

func PackageCatalog(catalogRoot, referenceBase string) error {
	result, err := internalcatalog.PackageCatalog(internalcatalog.PackageOptions{
		CatalogRoot:   catalogRoot,
		ReferenceBase: referenceBase,
	})
	if err != nil {
		return err
	}

	log.Info().Msgf(
		"Catalog %q version %q has been packaged in the local OCI cache as %s with digest %s",
		result.Manifest.Metadata.Name,
		result.Manifest.Spec.Version,
		result.Reference,
		result.Artifact.ManifestDigest,
	)
	return nil
}
