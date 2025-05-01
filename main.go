package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/juju/cmd/v3"
	"github.com/juju/errors"
	k8s "github.com/juju/juju/caas/kubernetes"
	k8sconstants "github.com/juju/juju/caas/kubernetes/provider/constants"
	jujucloud "github.com/juju/juju/cloud"
	"github.com/juju/juju/cmd/juju/common"
	"github.com/juju/juju/cmd/modelcmd"
	"github.com/juju/juju/controller"
	corebase "github.com/juju/juju/core/base"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/bootstrap"
	environscloudspec "github.com/juju/juju/environs/cloudspec"
	envcontext "github.com/juju/juju/environs/context"
	"github.com/juju/juju/environs/sync"
	"github.com/juju/juju/jujuclient"
	_ "github.com/juju/juju/provider/lxd"
	"github.com/juju/juju/state/stateenvirons"
	"github.com/juju/juju/storage"
	"github.com/juju/juju/storage/poolmanager"
	jujuversion "github.com/juju/juju/version"
)

type bootstrapConfigs struct {
	bootstrapModel           map[string]interface{}
	controller               controller.Config
	bootstrap                bootstrap.Config
	inheritedControllerAttrs map[string]interface{}
	userConfigAttrs          map[string]interface{}
	storagePools             map[string]storage.Attrs
}

// TODO: Add environ cleanup. See L1037.
func babybootstrap() error {
	bsf := bootstrapFuncs{}

	// 1. Get the cloud (Juju detects this or infers it from the provider.)
	//    We will get it sent via params. Example LXD cloud for testing is here.
	cloudDefinition := jujucloud.Cloud{
		Name:             "localhost",
		Type:             "lxd",
		HostCloudRegion:  "",
		Description:      "LXD Container Hypervisor",
		AuthTypes:        []jujucloud.AuthType{jujucloud.CertificateAuthType},
		Endpoint:         "",
		IdentityEndpoint: "",
		StorageEndpoint:  "",
		Regions: []jujucloud.Region{
			{
				Name:             "localhost",
				Endpoint:         "",
				IdentityEndpoint: "",
				StorageEndpoint:  "",
			},
		},
		Config:            nil,
		RegionConfig:      nil,
		CACertificates:    []string{},
		SkipTLSVerify:     false,
		IsControllerCloud: false,
	}

	// 2. Get the provider, to be detected from the cloud type. When Juju cannot
	// 	  find a cloud by name in the public cloud list, it attempts to detect it and
	//    create the provider. We will NOT be public cloud aware and will EXPECT the cloud
	//    to exist, if it doesn't, the request will fail. As such we can simply create the
	//    provider from the cloud type.
	provider, err := environs.Provider(cloudDefinition.Type)
	if err != nil {
		return err
	}

	// 3. After retrieving the provider, some providers need to "finalise" the cloud
	//    definition. Only K8S, EC2 and LXD do this. In the LXD provider this handles
	//    setting the Endpoint and region endpoint.
	if finalizer, ok := bsf.CloudFinalizer(provider); ok {
		cloudDefinition, err = finalizer.FinalizeCloud(finaliseCloudContext{}, cloudDefinition)
		if err != nil {
			return err
		}
	}

	// 4. Custom clouds may not have explicitly declared support for any auth-
	// 	  types, in which case we'll assume that they support everything that
	// 	  the provider supports.
	if len(cloudDefinition.AuthTypes) == 0 {
		for authType := range provider.CredentialSchemas() {
			cloudDefinition.AuthTypes = append(cloudDefinition.AuthTypes, authType)
		}
	}

	// 5. With our finalised cloud, we need the credential, in our case we'll have
	//    the credentials that we can look up by region.
	// store := jujuclient.NewMemStore()
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	os.Setenv("XDG_DATA_HOME", dir)
	store := jujuclient.NewFileClientStore()
	credential, credentialName, credentialRegionName, detected, err := common.GetOrDetectCredential(
		&cmd.Context{},
		store,
		provider,
		modelcmd.GetCredentialsParams{
			Cloud:          cloudDefinition,
			CloudRegion:    "",
			CredentialName: "",
		},
	)
	if err != nil {
		return err
	}
	_ = detected // Juju has a concept of detecting credentials. Dig into GetOrDetectCredential to learn more.

	// 6. Juju "Chooses" the cloud region, by first checking if the credential for this region
	//    can be found in the clouds regions. If not, it takes the first in the list. If none
	//    are available, a nameless region is created using the cloud itself's endpoint,
	//    identity endpoint and storage endpoint. This is the same as the "default" region.
	//
	//    For JIMM, I imagine we'll ALWAYS expect the region to exists for the credential's
	//    region name.
	cloudRegion, err := common.ChooseCloudRegion(cloudDefinition, credentialRegionName)
	if err != nil {
		return err
	}

	// 7. Next, the bootstrap configuration is created. Juju pulls the user's CLI flags
	//    and overrides provider & inherited configs. For this PoC, we're just getting
	//    the actual defaults and ignoring all user input.
	//
	//    I'm very unsure about this part, so it needs a good look over.
	bootstrapConfigs, err := getBoostrapConfigs(cloudDefinition, provider, cloudRegion.Name)
	if err != nil {
		return err
	}

	// 8. The bootstrap base is then determined utilising the image stream set
	//    within the bootstrapConfigs.
	//	  NOTE: This isn't used until much later, when we ACTUALLY bootstrap. It isn't used
	//    in the prepareParams.
	var bootstrapBase corebase.Base
	var imageStream string
	if cfg, ok := bootstrapConfigs.bootstrapModel["image-stream"]; ok {
		imageStream = cfg.(string)
	}
	now := time.Now()
	supportedBootstrapBases, err := corebase.ControllerBases(now, bootstrapBase, imageStream)
	if err != nil {
		return err
	}

	// 9. Set the controller name
	bootstrapConfigs.controller[controller.ControllerName] = "baby-bootstrap-controller"
	controllerName := bootstrapConfigs.controller[controller.ControllerName].(string)

	// 10. An interrupt handler is setup to cancel the bootstrap and clean up resources.
	//     As we don't actually have a command context, we're "faking it".
	//     This is then
	interrupted := make(chan os.Signal, 1)
	defer close(interrupted)
	var cmdCtx cmd.Context
	var stdCtx context.Context
	var cancel context.CancelFunc

	// Wild guessing this here, check at a later time if this works.
	cmdCtx = cmd.Context{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	stdCtx, cancel = context.WithTimeout(context.Background(), bootstrapConfigs.bootstrap.BootstrapTimeout)
	go func() {
		for range interrupted {
			select {
			case <-stdCtx.Done():
				// Ctrl-C already pressed
				return

			default:
				// Newline prefix is intentional, so output appears as
				// "^C\nCtrl-C pressed" instead of "^CCtrl-C pressed".
				_, _ = fmt.Fprintln(cmdCtx.GetStderr(), "\nCtrl-C pressed, attempting to stop bootstrap and clean up resources")
				cancel()
			}
		}
	}()

	// 11. A bootstrap context is created
	bootstrapCtx := modelcmd.BootstrapContext(stdCtx, &cmdCtx)

	// 12. Finally, the meat and potatoes, we create the "prepareParams" for bootstrap.
	//     And prepare the controller.
	// 	   It does a few things such as: update the store, on k8s it creates a namespace,
	//     checks credentials exist, for LXD specifically, found here
	//     provider/lxd/environ.go L71
	//     It sets the cloud spec. It can connecto remote servers (which may come in handy for JIMM testing!)
	//     It also creates an LXD profile. Each provider here is different in the way they apply their "cloud specs".
	bootstrapPrepareParams := bootstrap.PrepareParams{
		ModelConfig:      bootstrapConfigs.bootstrapModel,
		ControllerConfig: bootstrapConfigs.controller,
		ControllerName:   controllerName,
		Cloud: environscloudspec.CloudSpec{
			Type:             cloudDefinition.Type,
			Name:             cloudDefinition.Name,
			Region:           cloudRegion.Name,
			Endpoint:         cloudRegion.Endpoint,
			IdentityEndpoint: cloudRegion.IdentityEndpoint,
			StorageEndpoint:  cloudRegion.StorageEndpoint,
			Credential:       credential,
			CACertificates:   cloudDefinition.CACertificates,
			SkipTLSVerify:    cloudDefinition.SkipTLSVerify,
		},
		CredentialName: credentialName,
		AdminSecret:    bootstrapConfigs.bootstrap.AdminSecret,
	}
	isCAASController := jujucloud.CloudIsCAAS(cloudDefinition)
	deets, _ := store.AllControllers()
	_ = deets

	environ, err := bootstrap.PrepareController(isCAASController, bootstrapCtx, store, bootstrapPrepareParams)
	if err != nil {
		return err
	}

	// X. Validates the storage provider config, checking a pool can be created.
	//    Validate the storage provider config.
	//    Not strictly necessary.
	registry := stateenvirons.NewStorageProviderRegistry(environ)
	m := poolmanager.MemSettings{
		Settings: make(map[string]map[string]interface{}),
	}
	pm := poolmanager.New(m, registry)
	for poolName, cfg := range bootstrapConfigs.storagePools {
		poolType, _ := cfg[poolmanager.Type].(string)
		_, err = pm.Create(poolName, storage.ProviderType(poolType), cfg)
		if err != nil {
			return errors.NewNotValid(err, "invalid storage provider config")
		}
	}

	// 13. The bootstrap params are built up. This is the final step (plus some bits
	//     of validation and ensuring the params are correct) before running the bootstrap call.
	currentJujuVersion := jujuversion.Current
	charmChannel := fmt.Sprintf("%d.%d/stable", jujuversion.Current.Major, jujuversion.Current.Minor)
	charmChannelParsed, err := parseControllerCharmChannel(charmChannel)
	if err != nil {
		return err
	}

	bootstrapParams := bootstrap.BootstrapParams{
		ControllerName:            controllerName,
		BootstrapBase:             bootstrapBase,
		SupportedBootstrapBases:   supportedBootstrapBases,
		Cloud:                     cloudDefinition,
		CloudRegion:               cloudRegion.Name,
		ControllerConfig:          bootstrapConfigs.controller,
		ControllerInheritedConfig: bootstrapConfigs.inheritedControllerAttrs,
		RegionInheritedConfig:     cloudDefinition.RegionConfig,
		AdminSecret:               bootstrapConfigs.bootstrap.AdminSecret,
		CAPrivateKey:              bootstrapConfigs.bootstrap.CAPrivateKey,
		SSHServerHostKey:          bootstrapConfigs.bootstrap.SSHServerHostKey,
		ControllerServiceType:     bootstrapConfigs.bootstrap.ControllerServiceType,
		ControllerExternalName:    bootstrapConfigs.bootstrap.ControllerExternalName,
		ControllerExternalIPs:     append([]string(nil), bootstrapConfigs.bootstrap.ControllerExternalIPs...),
		DialOpts: environs.BootstrapDialOpts{
			Timeout:        bootstrapConfigs.bootstrap.BootstrapTimeout,
			RetryDelay:     bootstrapConfigs.bootstrap.BootstrapRetryDelay,
			AddressesDelay: bootstrapConfigs.bootstrap.BootstrapAddressesDelay,
		},
		StoragePools: bootstrapConfigs.storagePools,
		// These fields are not gathered from the configs, cloud definition or cloud region.
		// Most are set from flags and would require validation to implement.
		BootstrapImage:           "",
		Placement:                "",
		BuildAgent:               false,
		BuildAgentTarball:        sync.BuildAgentTarball,
		AgentVersion:             &currentJujuVersion, // Cannot be used with build agent. Also has some complexity, easiest case is this.
		JujuDbSnapPath:           "",                  // Path to a locally built .snap to use as the internal juju-db service
		JujuDbSnapAssertionsPath: "",                  // Path to a local .assert file. Requires --db-snap
		ControllerCharmPath:      "",                  // Path to a locally built controller charm
		ControllerCharmChannel:   charmChannelParsed,
		Force:                    false, // Allow the bypassing of checks such as supported series
		// The credential is actually set in L1092 & L1093
		// but our usecase will always know the credential (ideally?) as we wouldn't be detecting it.
		CloudCredential:     credential,
		CloudCredentialName: credentialName,
	}

	// 14. Set initialise the first model (in addition to the controller model).
	//     The juju code actually does the following:
	// 			if c.initialModelName == "" || c.initialModelName == "controller" {
	// 				// Nothing to do, but ensure the required model is selected by default.
	// 				return nil, store.SetCurrentModel(c.controllerName, c.initialModelName)
	// 			}
	//
	// But the "SetCurrentModel" does not allow me to set "controller".
	// TODO: In our usecase, this will need supporting.
	initialModelName := ""
	if err := store.SetCurrentModel(controllerName, initialModelName); err != nil {
		return err
	}
	if err := store.SetCurrentController(controllerName); err != nil {
		return err
	}

	// 15. Setup a cloud call context with a invalid credential function.
	cloudCallCtx := envcontext.NewCloudCallContext(stdCtx)
	cloudCallCtx.InvalidateCredentialFunc = func(reason string) error {
		fmt.Println("invalid credential", credentialName, reason)
		return err
	}

	// 16. Set a metadata source for specifying the local simple streams source to use with juju.
	//     This is not something we will do as we'll always use upstream simple streams, at least
	//     I cannot think of a reason we would want to do this.

	// 17. Constraints are set up, a validator and two constraint strings as seen in L604.
	//     For this PoC, constraints are not implemented
	//     TODO allow constraints strings.
	//
	//     See line 1057 to 1084.

	// 18. Not sure why we print this, but it is in the original code.
	if cloudDefinition.Type == k8sconstants.CAASProviderType {
		if cloudDefinition.HostCloudRegion == k8s.K8sCloudOther {
			fmt.Println("Bootstrap to generic Kubernetes cluster")
		} else {
			fmt.Printf("Bootstrap to Kubernetes cluster identified as %s \n", cloudDefinition.HostCloudRegion)
		}
	}

	// 19. Now we can bootstrap!
	if err := bsf.Bootstrap(
		bootstrapCtx,
		environ,
		cloudCallCtx,
		bootstrapParams,
	); err != nil {
		fmt.Println("failed oh noes")
		return err
	}

	// 20. A controller data refresher is created which writes any new api addresses and other relevant details to the client's controller file.
	if err = controllerDataRefresher(
		store, currentJujuVersion, controllerName, // Additional params I added
		environ,
		cloudCallCtx,
		bootstrapConfigs,
	); err != nil {
		return errors.Trace(err)
	}

	// 21. Model access is ensured, needs implementing.
	// 22. We wait for the agent initialisation to complete.
	//     It uses modelcmd ClientStore() and NewAPIRootWithDialOpts() and some other calls (which reference the store)
	//     which is a bit annoying, this should be changed in Juju at a future date for our bootstrap.
	cmdBase := &modelcmd.ModelCommandBase{}
	cmdBase.SetClientStore(store)
	err = common.WaitForAgentInitialisation(
		bootstrapCtx,
		cmdBase,
		isCAASController,
		controllerName,
	)
	if err != nil {
		fmt.Println("wait for agent initialisation failed")
		return err
	}
	_ = bootstrapParams
	return nil
}

func main() {
	err := babybootstrap()
	if err != nil {
		log.Fatal(err)
	}
}
