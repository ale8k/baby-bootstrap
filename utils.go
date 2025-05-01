package main

import (
	"fmt"

	"github.com/juju/charm/v12"
	"github.com/juju/errors"
	"github.com/juju/juju/caas"
	k8sconstants "github.com/juju/juju/caas/kubernetes/provider/constants"
	"github.com/juju/juju/cmd/juju/common"
	"github.com/juju/juju/core/network"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/bootstrap"
	envcontext "github.com/juju/juju/environs/context"
	"github.com/juju/juju/juju"
	"github.com/juju/juju/jujuclient"
	"github.com/juju/juju/proxy"
	jujuversion "github.com/juju/juju/version"
	"github.com/juju/version/v2"
)

type bootstrapFuncs struct{}

func (b bootstrapFuncs) Bootstrap(ctx environs.BootstrapContext, env environs.BootstrapEnviron,
	callCtx envcontext.ProviderCallContext, args bootstrap.BootstrapParams) error {
	return bootstrap.Bootstrap(ctx, env, callCtx, args)
}

func (b bootstrapFuncs) CloudDetector(provider environs.EnvironProvider) (environs.CloudDetector, bool) {
	detector, ok := provider.(environs.CloudDetector)
	return detector, ok
}

func (b bootstrapFuncs) CloudRegionDetector(provider environs.EnvironProvider) (environs.CloudRegionDetector, bool) {
	detector, ok := provider.(environs.CloudRegionDetector)
	return detector, ok
}

func (b bootstrapFuncs) CloudFinalizer(provider environs.EnvironProvider) (environs.CloudFinalizer, bool) {
	finalizer, ok := provider.(environs.CloudFinalizer)
	return finalizer, ok
}

type finaliseCloudContext struct {
}

func (fcc finaliseCloudContext) Verbosef(msg string, args ...interface{}) {
	fmt.Printf(msg, args...)
}

func parseControllerCharmChannel(channelStr string) (charm.Channel, error) {
	ch, err := charm.ParseChannel(channelStr)
	if err != nil {
		return charm.Channel{}, err
	}

	if ch.Track == "" {
		ch.Track = fmt.Sprintf("%d.%d", jujuversion.Current.Major, jujuversion.Current.Minor)
	}
	if ch.Risk == "" {
		ch.Risk = charm.Stable
	}
	return ch, nil
}

func controllerDataRefresher(
	store jujuclient.ControllerStore,
	agentVersion version.Number,
	controllerName string,
	environ environs.BootstrapEnviron,
	cloudCallCtx *envcontext.CloudCallContext,
	bootstrapCfg bootstrapConfigs,
) error {

	// This logic allows polling for address info later during retries,
	// for example, when a load balancer needs time to be provisioned.
	var addrs []network.ProviderAddress
	var err error
	if env, ok := environ.(environs.InstanceBroker); ok {
		// IAAS.
		addrs, err = common.BootstrapEndpointAddresses(env, cloudCallCtx)
		if err != nil {
			return errors.Trace(err)
		}
	} else if env, ok := environ.(caas.ServiceManager); ok {
		// CAAS.
		var svc *caas.Service
		svc, err = env.GetService(k8sconstants.JujuControllerStackName, caas.ModeWorkload, false)
		if err != nil {
			return errors.Trace(err)
		}
		if len(svc.Addresses) == 0 {
			return errors.NotProvisionedf("k8s controller service %q address", svc.Id)
		}
		addrs = svc.Addresses
	} else {
		// This should never happen.
		return errors.New(
			"supplied BootstrapEnviron implements neither environs.InstanceBroker nor caas.ServiceGetterSetter")
	}

	var proxier proxy.Proxier
	if conInfo, ok := environ.(environs.ConnectorInfo); ok {
		proxier, err = conInfo.ConnectionProxyInfo()
		if err != nil && !errors.IsNotFound(err) {
			return errors.Trace(err)
		}
	}

	// Use the retrieved bootstrap machine/service addresses to create
	// host/port endpoints for local storage.
	hps := make([]network.MachineHostPort, len(addrs))
	for i, addr := range addrs {
		hps[i] = network.MachineHostPort{
			MachineAddress: addr.MachineAddress,
			NetPort:        network.NetPort(bootstrapCfg.controller.APIPort()),
		}
	}
	return errors.Annotate(
		juju.UpdateControllerDetailsFromLogin(
			store,
			controllerName,
			juju.UpdateControllerParams{
				AgentVersion:           agentVersion.String(),
				CurrentHostPorts:       []network.MachineHostPorts{hps},
				PublicDNSName:          newStringIfNonEmpty(bootstrapCfg.controller.AutocertDNSName()),
				MachineCount:           newInt(1),
				Proxier:                proxier,
				ControllerMachineCount: newInt(1),
			},
		),
		"saving bootstrap endpoint address",
	)
}
func newStringIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func newInt(i int) *int {
	return &i
}
