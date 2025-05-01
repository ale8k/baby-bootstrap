package main

import (
	"github.com/juju/cmd/v3"
	jujucloud "github.com/juju/juju/cloud"
	"github.com/juju/juju/cmd/juju/common"
	"github.com/juju/juju/controller"
	"github.com/juju/juju/docker"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/bootstrap"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/storage"
	"github.com/juju/schema"
	"github.com/juju/utils/v3"
)

func getBoostrapConfigs(cloudDefinition jujucloud.Cloud, provider environs.EnvironProvider, cloudRegionName string) (bootstrapConfigs, error) {
	controllerModelUUID, err := utils.NewUUID()
	if err != nil {
		return bootstrapConfigs{}, err
	}
	controllerUUID, err := utils.NewUUID()
	if err != nil {
		return bootstrapConfigs{}, err
	}

	providerAttrs := make(map[string]interface{})
	bootstrapConfigAttrs := make(map[string]interface{})
	controllerConfigAttrs := make(map[string]interface{})
	inheritedControllerAttrs := make(map[string]interface{})

	// Set provider attributes. (Missing bits from original code)
	if ps, ok := provider.(config.ConfigSchemaSource); ok {
		fields := schema.FieldMap(ps.ConfigSchema(), ps.ConfigDefaults())
		coercedAttrs, _ := fields.Coerce(providerAttrs, nil)
		providerAttrs = coercedAttrs.(map[string]interface{})
	}

	// Set cloud definition config attributes.
	for k, v := range cloudDefinition.Config {
		switch {
		case bootstrap.IsBootstrapAttribute(k):
			bootstrapConfigAttrs[k] = v
			continue
		case controller.ControllerOnlyAttribute(k):
			controllerConfigAttrs[k] = v
			continue
		}
		inheritedControllerAttrs[k] = v
	}

	for k, v := range cloudDefinition.RegionConfig[cloudRegionName] {
		switch {
		case bootstrap.IsBootstrapAttribute(k):
			bootstrapConfigAttrs[k] = v
			continue
		case controller.ControllerOnlyAttribute(k):
			controllerConfigAttrs[k] = v
			continue
		}
		inheritedControllerAttrs[k] = v
	}

	// Create a model config, and split out any controller
	// and bootstrap config attributes.
	combinedConfig := map[string]interface{}{
		"type":         cloudDefinition.Type,
		"name":         bootstrap.ControllerModelName,
		config.UUIDKey: controllerModelUUID.String(),
	}

	for k, v := range providerAttrs {
		combinedConfig[k] = v
	}

	for k, v := range inheritedControllerAttrs {
		combinedConfig[k] = v
	}

	for k, v := range config.ConfigDefaults() {
		if _, ok := combinedConfig[k]; !ok {
			combinedConfig[k] = v
		}
	}

	bootstrapModelConfig := make(map[string]interface{})
	for k, v := range combinedConfig {
		switch {
		case bootstrap.IsBootstrapAttribute(k):
			bootstrapConfigAttrs[k] = v
		case controller.ControllerOnlyAttribute(k):
			controllerConfigAttrs[k] = v
		default:
			bootstrapModelConfig[k] = v
		}
	}

	bootstrapConfig, err := bootstrap.NewConfig(bootstrapConfigAttrs)
	if err != nil {
		return bootstrapConfigs{}, err
	}

	if v, ok := controllerConfigAttrs[controller.CAASImageRepo]; ok {
		if v, ok := v.(string); ok {
			// Wouldn't work for us, but this is how Juju handles it locally.
			repoDetails, err := docker.LoadImageRepoDetails(v)
			if err != nil {
				return bootstrapConfigs{}, err
			}
			controllerConfigAttrs[controller.CAASImageRepo] = repoDetails.Content()
		}
	}

	controllerConfig, err := controller.NewConfig(
		controllerUUID.String(),
		bootstrapConfig.CACert,
		controllerConfigAttrs,
	)
	if err != nil {
		return bootstrapConfigs{}, err
	}

	if controllerConfig.AutocertDNSName() != "" {
		if _, ok := controllerConfigAttrs[controller.APIPort]; !ok {
			// The configuration did not explicitly mention the API port,
			// so default to 443 because it is not usually possible to
			// obtain autocert certificates without listening on port 443.
			controllerConfig[controller.APIPort] = 443
		}
	}

	if err := common.FinalizeAuthorizedKeys(&cmd.Context{}, bootstrapModelConfig); err != nil {
		return bootstrapConfigs{}, err
	}
	// TODO: L1694, Azure specific check.

	// TODO: User inputs & storage pools.
	storagePools := make(map[string]storage.Attrs)
	userConfigAttrs := make(map[string]any)

	configs := bootstrapConfigs{
		bootstrapModel:           bootstrapModelConfig,
		controller:               controllerConfig,
		bootstrap:                bootstrapConfig,
		inheritedControllerAttrs: inheritedControllerAttrs,
		userConfigAttrs:          userConfigAttrs,
		storagePools:             storagePools,
	}

	return configs, nil
}
