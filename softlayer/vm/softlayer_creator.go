package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"text/template"
	"time"
	"strings"

	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"

	common "github.com/maximilien/bosh-softlayer-cpi/common"
	bslcommon "github.com/maximilien/bosh-softlayer-cpi/softlayer/common"
	bslcstem "github.com/maximilien/bosh-softlayer-cpi/softlayer/stemcell"
	bslcvmpool "github.com/maximilien/bosh-softlayer-cpi/softlayer/vm/pool"
	sl "github.com/maximilien/softlayer-go/softlayer"

	boshsys "github.com/cloudfoundry/bosh-utils/system"

	util "github.com/maximilien/bosh-softlayer-cpi/util"
)

const softLayerCreatorLogTag = "SoftLayerCreator"

type SoftLayerCreator struct {
	softLayerClient        sl.Client
	agentEnvServiceFactory AgentEnvServiceFactory

	agentOptions AgentOptions
	logger       boshlog.Logger

	OsReloadTimeout time.Duration
}

func NewSoftLayerCreator(softLayerClient sl.Client, agentEnvServiceFactory AgentEnvServiceFactory, agentOptions AgentOptions, logger boshlog.Logger) SoftLayerCreator {
	bslcommon.TIMEOUT = 60 * time.Minute
	bslcommon.POLLING_INTERVAL = 10 * time.Second

	return SoftLayerCreator{
		softLayerClient:        softLayerClient,
		agentEnvServiceFactory: agentEnvServiceFactory,
		agentOptions:           agentOptions,
		logger:                 logger,
		OsReloadTimeout:        30 * time.Second,
	}
}

func (c SoftLayerCreator) CreateNewVM(agentID string, stemcell bslcstem.Stemcell, cloudProps VMCloudProperties, networks Networks, env Environment) (VM, error) {
	virtualGuestTemplate, err := CreateVirtualGuestTemplate(agentID, stemcell, cloudProps, networks, env, c.agentOptions)
	if err != nil {
		return SoftLayerVM{}, bosherr.WrapError(err, "Creating virtual guest template")
	}

	virtualGuestService, err := c.softLayerClient.GetSoftLayer_Virtual_Guest_Service()
	if err != nil {
		return SoftLayerVM{}, bosherr.WrapError(err, "Creating VirtualGuestService from SoftLayer client")
	}

	virtualGuest, err := virtualGuestService.CreateObject(virtualGuestTemplate)
	if err != nil {
		return SoftLayerVM{}, bosherr.WrapError(err, "Creating VirtualGuest from SoftLayer client")
	}

	if cloudProps.EphemeralDiskSize == 0 {
		err = bslcommon.WaitForVirtualGuest(c.softLayerClient, virtualGuest.Id, "RUNNING")
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapError(err, fmt.Sprintf("PowerOn failed with VirtualGuest id `%d`", virtualGuest.Id))
		}
	} else {
		err = bslcommon.AttachEphemeralDiskToVirtualGuest(c.softLayerClient, virtualGuest.Id, cloudProps.EphemeralDiskSize, c.logger)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapError(err, fmt.Sprintf("Attaching ephemeral disk to VirtualGuest `%d`", virtualGuest.Id))
		}
	}

	agentEnvService := c.agentEnvServiceFactory.New(virtualGuest.Id)
	vm := NewSoftLayerVM(virtualGuest.Id, c.softLayerClient, util.GetSshClient(), agentEnvService, c.logger)

	if len(cloudProps.BoshIp) == 0 {
		virtualGuest, err = bslcommon.GetObjectDetailsOnVirtualGuest(vm.softLayerClient, virtualGuest.Id)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapErrorf(err, "Cannot get details from virtual guest with id: %d.", virtualGuest.Id)
		}
		c.updateEtcHostsOfBoshInit(fmt.Sprintf("%s  %s", virtualGuest.PrimaryBackendIpAddress, virtualGuest.FullyQualifiedDomainName))

		// Update mbus url setting for bosh director
		metadata, err := bslcommon.GetUserMetadataOnVirtualGuest(vm.softLayerClient, virtualGuest.Id)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapErrorf(err, "Cannot get metadata from virtual guest with id: %d.", virtualGuest.Id)
		}
		agentEnv, err := NewAgentEnvFromJSON(metadata)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapErrorf(err, "Cannot unmarshal metadata from virutal guest with id: %d.", virtualGuest.Id)
		}

		//Construct mbus url with new director ip
		mbus, err := c.parseMbusURL(c.agentOptions.Mbus, virtualGuest.PrimaryBackendIpAddress)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapErrorf(err, "Cannot construct mbus url.")
		}
		agentEnv.Mbus = mbus

		metadata, err = json.Marshal(agentEnv)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapError(err, "Marshalling agent environment metadata")
		}

		err = bslcommon.ConfigureMetadataOnVirtualGuest(vm.softLayerClient, virtualGuest.Id, string(metadata), c.logger)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapError(err, fmt.Sprintf("Configuring metadata on VirtualGuest `%d`", virtualGuest.Id))
		}
	}

	return vm, nil
}

func (c SoftLayerCreator) Create(agentID string, stemcell bslcstem.Stemcell, cloudProps VMCloudProperties, networks Networks, env Environment) (VM, error) {
	if strings.ToUpper(common.GetOSEnvVariable("OS_RELOAD_ENABLED", "TRUE")) == "FALSE" {
		return c.CreateNewVM(agentID, stemcell, cloudProps, networks, env)
	}

	if strings.Contains(cloudProps.VmNamePrefix, "-worker") {
		vm, err := c.CreateNewVM(agentID, stemcell, cloudProps, networks, env)
		return vm, err
	}

	err := bslcvmpool.InitVMPoolDB(bslcvmpool.DB_RETRY_TIMEOUT, bslcvmpool.DB_RETRY_INTERVAL, c.logger)
	if err != nil {
		return SoftLayerVM{}, bosherr.WrapError(err, "Failed to initialize VM pool DB")
	}

	db, err := bslcvmpool.OpenDB(bslcvmpool.SQLITE_DB_FILE_PATH)
	if err != nil {
		return SoftLayerVM{}, bosherr.WrapError(err, "Opening DB")
	}

	vmInfoDB := bslcvmpool.NewVMInfoDB(0, "", "f", "", agentID, c.logger, db)
	defer vmInfoDB.CloseDB()

	err = vmInfoDB.QueryVMInfobyAgentID(bslcvmpool.DB_RETRY_TIMEOUT, bslcvmpool.DB_RETRY_INTERVAL)
	if err != nil {
		return SoftLayerVM{}, bosherr.WrapError(err, "Failed to query VM info by given agent ID "+agentID)
	}

	if vmInfoDB.VmProperties.Id != 0 {
		c.logger.Info(softLayerCreatorLogTag, fmt.Sprintf("OS reload on the server id %d with stemcell %d", vmInfoDB.VmProperties.Id, stemcell.ID()))

		agentEnvService := c.agentEnvServiceFactory.New(vmInfoDB.VmProperties.Id)
		vm := NewSoftLayerVM(vmInfoDB.VmProperties.Id, c.softLayerClient, util.GetSshClient(), agentEnvService, c.logger)

		vm.ReloadOS(stemcell, c.OsReloadTimeout)
		if err != nil {
			return SoftLayerVM{}, bosherr.WrapError(err, "Failed to reload OS")
		}

		c.logger.Info(softLayerCreatorLogTag, fmt.Sprintf("Updated in_use flag to 't' for the VM %d in VM pool", vmInfoDB.VmProperties.Id))
		vmInfoDB.VmProperties.InUse = "t"
		err = vmInfoDB.UpdateVMInfoByID(bslcvmpool.DB_RETRY_TIMEOUT, bslcvmpool.DB_RETRY_INTERVAL)
		if err != nil {
			return vm, bosherr.WrapError(err, fmt.Sprintf("Failed to query VM info by given ID %d", vm.ID()))
		} else {
			return vm, nil
		}
	}

	vmInfoDB.VmProperties.InUse = ""
	err = vmInfoDB.QueryVMInfobyAgentID(bslcvmpool.DB_RETRY_TIMEOUT, bslcvmpool.DB_RETRY_INTERVAL)
	if err != nil {
		return SoftLayerVM{}, bosherr.WrapError(err, "Failed to query VM info by given agent ID "+agentID)
	}

	if vmInfoDB.VmProperties.Id != 0 {
		return SoftLayerVM{}, bosherr.WrapError(err, "Wrong in_use status in VM with agent ID "+agentID+", Do not create a new VM")
	} else {
		return c.CreateNewVM(agentID, stemcell, cloudProps, networks, env)
	}

}

// Private methods

func (c SoftLayerCreator) resolveNetworkIP(networks Networks) (string, error) {
	var network Network

	switch len(networks) {
	case 0:
		return "", bosherr.Error("Expected exactly one network; received zero")
	case 1:
		network = networks.First()
	default:
		return "", bosherr.Error("Expected exactly one network; received multiple")
	}

	if network.IsDynamic() {
		return "", nil
	}

	return network.IP, nil
}

func (c SoftLayerCreator) parseMbusURL(mbusURL string, primaryBackendIpAddress string) (string, error) {
	parsedURL, err := url.Parse(mbusURL)
	if err != nil {
		return "", bosherr.WrapError(err, "Parsing Mbus URL")
	}
	var username, password, port string
	_, port, _ = net.SplitHostPort(parsedURL.Host)
	userInfo := parsedURL.User
	if userInfo != nil {
		username = userInfo.Username()
		password, _ = userInfo.Password()
		return fmt.Sprintf("https://%s:%s@%s:%s", username, password, primaryBackendIpAddress, port), nil
	}

	return fmt.Sprintf("https://%s:%s", primaryBackendIpAddress, port), nil
}

func (c SoftLayerCreator) updateEtcHostsOfBoshInit(record string) (err error) {
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("etc-hosts").Parse(ETC_HOSTS_TEMPLATE))

	err = t.Execute(buffer, record)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	fileSystem := boshsys.NewOsFileSystemWithStrictTempRoot(c.logger)
	err = fileSystem.WriteFile("/etc/hosts", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/hosts")
	}

	return nil
}

const ETC_HOSTS_TEMPLATE = `127.0.0.1 localhost
{{.}}
`
