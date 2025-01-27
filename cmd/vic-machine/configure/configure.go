// Copyright 2017 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package configure

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/urfave/cli.v1"

	"github.com/docker/go-units"

	"github.com/vmware/vic/cmd/vic-machine/common"
	"github.com/vmware/vic/lib/config"
	"github.com/vmware/vic/lib/config/executor"
	"github.com/vmware/vic/lib/install/data"
	"github.com/vmware/vic/lib/install/management"
	"github.com/vmware/vic/lib/install/validate"
	"github.com/vmware/vic/pkg/errors"
	"github.com/vmware/vic/pkg/trace"
	"github.com/vmware/vic/pkg/version"
	"github.com/vmware/vic/pkg/vsphere/vm"
)

// Configure has all input parameters for vic-machine configure command
type Configure struct {
	*data.Data

	proxies    common.Proxies
	cNetworks  common.CNetworks
	dns        common.DNS
	volStores  common.VolumeStores
	registries common.Registries

	certificates common.CertFactory

	executor *management.Dispatcher

	Force bool
	help  common.Help
}

func NewConfigure() *Configure {
	configure := &Configure{}
	configure.Data = data.NewData()

	return configure
}

// Flags return all cli flags for configure
func (c *Configure) Flags() []cli.Flag {
	util := []cli.Flag{
		cli.BoolFlag{
			Name:        "force, f",
			Usage:       "Force the configure operation",
			Destination: &c.Force,
		},
		cli.DurationFlag{
			Name:        "timeout",
			Value:       3 * time.Minute,
			Usage:       "Time to wait for configure",
			Destination: &c.Timeout,
		},
		cli.BoolFlag{
			Name:        "reset-progress",
			Usage:       "Reset the UpdateInProgress flag. Warning: Do not reset this flag if another upgrade/configure process is running",
			Destination: &c.ResetInProgressFlag,
		},
		cli.BoolFlag{
			Name:        "rollback",
			Usage:       "Roll back VCH configuration to before the current upgrade/configure",
			Destination: &c.Rollback,
			Hidden:      true,
		},
	}

	dns := c.dns.DNSFlags()
	target := c.TargetFlags()
	ops := c.OpsCredentials.Flags()
	id := c.IDFlags()
	volume := c.volStores.Flags()
	compute := c.ComputeFlags()
	affinity := c.AffinityFlags()
	container := c.ContainerFlags()
	debug := c.DebugFlags(true)
	cNetwork := c.cNetworks.CNetworkFlags()
	proxies := c.proxies.ProxyFlags()
	memory := c.VCHMemoryLimitFlags()
	cpu := c.VCHCPULimitFlags()
	certificates := c.certificates.CertFlags()
	registries := c.registries.Flags()
	help := c.help.HelpFlags()
	squota := c.VCHStorageQuotaFlag()
	cvms := c.VCHContainerCountFlag()

	// flag arrays are declared, now combined
	var flags []cli.Flag
	for _, f := range [][]cli.Flag{target, ops, id, compute, affinity, container, volume, dns, cNetwork, memory, cpu, squota, cvms, certificates, registries, proxies, util, debug, help} {
		flags = append(flags, f...)
	}

	return flags
}

func (c *Configure) processParams(op trace.Operation) error {
	defer trace.End(trace.Begin("", op))

	if err := c.HasCredentials(op); err != nil {
		return err
	}

	var err error
	if c.DNS, err = c.dns.ProcessDNSServers(op); err != nil {
		return err
	}

	hproxy, sproxy, nproxy, err := c.proxies.ProcessProxies()
	if err != nil {
		return err
	}
	c.HTTPProxy = hproxy
	c.HTTPSProxy = sproxy
	c.NoProxy = nproxy

	c.ProxyIsSet = c.proxies.IsSet

	c.ContainerNetworks, err = c.cNetworks.ProcessContainerNetworks(op)
	if err != nil {
		return err
	}

	// Pass empty admin credentials because they are needed only for a create
	// operation for use as ops credentials if ops credentials are not supplied.
	if err := c.OpsCredentials.ProcessOpsCredentials(op, false, "", nil); err != nil {
		return err
	}

	c.VolumeLocations, err = c.volStores.ProcessVolumeStores()
	if err != nil {
		return err
	}

	if err := c.registries.ProcessRegistries(op); err != nil {
		return err
	}
	c.Data.RegistryCAs = c.registries.RegistryCAs

	return nil
}

// copyChangedConf takes the mostly-empty new config and copies it to the old one. NOTE: o gets installed on the VCH, not n
// Currently we cannot automatically override old configuration with any difference in the new configuration, because some options are set during the VCH
// Creation process, for example, image store path, volume store path, network slot id, etc. So we'll copy changes based on user input
func (c *Configure) copyChangedConf(o *config.VirtualContainerHostConfigSpec, n *config.VirtualContainerHostConfigSpec, clic *cli.Context, v *validate.Validator) {
	//TODO: copy changed data
	personaSession := o.ExecutorConfig.Sessions[config.PersonaService]
	vicAdminSession := o.ExecutorConfig.Sessions[config.VicAdminService]
	portlayerSession := o.ExecutorConfig.Sessions[config.PortLayerService]
	if c.proxies.IsSet {
		if c.proxies.HTTPProxy != nil {
			hProxy := *c.proxies.HTTPProxy
			updateSessionEnv(personaSession, config.GeneralHTTPProxy, hProxy)
			updateSessionEnv(vicAdminSession, config.VICAdminHTTPProxy, hProxy)
		}
		if c.proxies.HTTPSProxy != nil {
			sProxy := *c.proxies.HTTPSProxy
			updateSessionEnv(personaSession, config.GeneralHTTPSProxy, sProxy)
			updateSessionEnv(vicAdminSession, config.VICAdminHTTPSProxy, sProxy)
		}
	}

	if c.proxies.NoProxy != nil {
		nProxy := ""
		if *c.proxies.NoProxy != "" {
			// prefer to whitespace trimmed format
			nProxy = *c.NoProxy
		}
		updateSessionEnv(personaSession, config.GeneralNoProxy, nProxy)
		updateSessionEnv(vicAdminSession, config.VICAdminNoProxy, nProxy)
	}

	// update renamed datacenter or cluster into session
	if c.ComputeResourcePath != "" {
		updateSessionArgs(vicAdminSession, config.VICAdminDCPath, v.Session().Datacenter.Name())
		updateSessionArgs(vicAdminSession, config.VICAdminPoolPath, v.Session().PoolPath)
		updateSessionArgs(vicAdminSession, config.VICAdminCSPath, v.Session().Cluster.InventoryPath)

		updateSessionEnv(portlayerSession, config.PortLayerCSPath, v.Session().Cluster.InventoryPath)
		updateSessionEnv(portlayerSession, config.PortLayerPoolPath, v.Session().PoolPath)
		updateSessionEnv(portlayerSession, config.PortLayerDCPath, v.Session().Datacenter.Name())
	}

	if c.Debug.Debug != nil {
		o.SetDebug(n.Diagnostics.DebugLevel)
	}

	if c.cNetworks.IsSet {
		o.ContainerNetworks = n.ContainerNetworks
	}

	if c.Data.ContainerNameConvention != "" {
		o.ContainerNameConvention = c.Data.ContainerNameConvention
	}

	// Copy the new volume store configuration directly since it has the merged
	// volume store configuration and its datastore URL fields have been populated
	// correctly by the storage validator. The old configuration has raw fields.
	o.VolumeLocations = n.VolumeLocations

	if c.OpsCredentials.IsSet {
		o.Username = n.Username
		o.Token = n.Token

		// if the user explicitly set the `ops-grant-user` option, update the permissions level
		if clic.IsSet("ops-grant-perms") {
			o.GrantPermsLevel = n.GrantPermsLevel
		}
	}

	// Copy the thumbprint directly since it has already been validated.
	o.TargetThumbprint = n.TargetThumbprint

	if c.dns.IsSet {
		for k, v := range o.ExecutorConfig.Networks {
			v.Network.Nameservers = n.ExecutorConfig.Networks[k].Network.Nameservers
			var gw net.IPNet
			v.Network.Assigned.Gateway = gw
			v.Network.Assigned.Nameservers = nil
		}
	}

	if n.HostCertificate != nil {
		o.HostCertificate = n.HostCertificate
	}

	if n.CertificateAuthorities != nil {
		o.CertificateAuthorities = n.CertificateAuthorities
	}

	if n.UserCertificates != nil {
		o.UserCertificates = n.UserCertificates
	}

	if n.RegistryCertificateAuthorities != nil {
		o.RegistryCertificateAuthorities = n.RegistryCertificateAuthorities
	}

	o.UseVMGroup = n.UseVMGroup

	if n.VMGroupName != "" {
		// If we're disabling use of a VM Group, we need to keep track of the name so that we can delete it. This has a
		// side effect of leaving behind the old VMGroupName value in the VCH's configuration, but it will not be used.
		o.VMGroupName = n.VMGroupName
	}
}

func updateSessionEnv(sess *executor.SessionConfig, envName, envValue string) {
	envs := sess.Cmd.Env
	var newEnvs []string
	for _, env := range envs {
		if strings.HasPrefix(env, envName+"=") {
			continue
		}
		newEnvs = append(newEnvs, env)
	}
	if envValue != "" {
		newEnvs = append(newEnvs, fmt.Sprintf("%s=%s", envName, envValue))
	}
	sess.Cmd.Env = newEnvs
}

func updateSessionArgs(sess *executor.SessionConfig, argName, argValue string) {
	args := sess.Cmd.Args
	var newArgs []string
	for _, arg := range args {
		if strings.HasPrefix(arg, argName+"=") {
			continue
		}
		newArgs = append(newArgs, arg)
	}
	if argValue != "" {
		newArgs = append(newArgs, fmt.Sprintf("%s=%s", argName, argValue))
	}
	sess.Cmd.Args = newArgs
}

func (c *Configure) processCertificates(op trace.Operation, client, public, management data.NetworkConfig) error {

	if c.certificates.Cname == "" && !c.certificates.NoTLSverify && (c.certificates.Skey == "" || c.certificates.Scert == "") {
		op.Info("No certificate regeneration requested. No new certificates provided. Certificates left unchanged.")
		return nil
	}

	if c.certificates.CertPath == "" {
		c.certificates.CertPath = c.DisplayName
	}

	_, err := os.Lstat(c.certificates.CertPath)
	if err == nil || os.IsExist(err) {
		return fmt.Errorf("Specified or default certificate output location \"%s\" already exists. Specify a location that does not yet exist with --tls-cert-path to continue or do not specify --tls-noverify if, instead, you want to load certificates from %s", c.certificates.CertPath, c.certificates.CertPath)
	}

	var debug int
	if c.Debug.Debug == nil {
		debug = 0
	} else {
		debug = *c.Debug.Debug
	}

	c.certificates.Networks = common.Networks{
		ClientNetworkName:     client.Name,
		ClientNetworkIP:       client.IP.String(),
		PublicNetworkName:     public.Name,
		PublicNetworkIP:       public.IP.String(),
		ManagementNetworkName: management.Name,
		ManagementNetworkIP:   management.IP.String(),
	}

	if err := c.certificates.ProcessCertificates(op, c.DisplayName, c.Force, debug); err != nil {
		return err
	}

	c.KeyPEM = c.certificates.KeyPEM
	c.CertPEM = c.certificates.CertPEM
	c.ClientCAs = c.certificates.ClientCAs
	return nil
}

func (c *Configure) Run(clic *cli.Context) (err error) {

	if c.help.Print(clic) {
		return nil
	}

	parentOp := common.NewOperation(clic, c.Debug.Debug)
	defer func(op trace.Operation) {
		// urfave/cli will print out exit in error handling, so no more information in main method can be printed out.
		err = common.LogErrorIfAny(op, clic, err)
	}(parentOp)
	op, cancel := trace.WithTimeout(&parentOp, c.Timeout, clic.App.Name)
	defer cancel()
	defer func() {
		if op.Err() != nil && op.Err() == context.DeadlineExceeded {
			//context deadline exceeded, replace returned error message
			err = errors.Errorf("Configure timed out: use --timeout to add more time")
		}
	}()

	// process input parameters, this should reuse same code with create command, to make sure same options are provided
	if err = c.processParams(op); err != nil {
		return err
	}

	if len(clic.Args()) > 0 {
		op.Errorf("Unknown argument: %s", clic.Args()[0])
		return errors.New("invalid CLI arguments")
	}

	op.Infof("### Configuring VCH ####")
	op.Debugf("vic-machine version %s", version.GetBuild().ShortVersion())

	validator, err := validate.NewValidator(op, c.Data)
	if err != nil {
		op.Errorf("Configure cannot continue - failed to create validator: %s", err)
		return errors.New("configure failed")
	}
	defer validator.Session().Logout(parentOp) // parentOp is used here to ensure the logout occurs, even in the event of timeout

	updatedStorageQuota := c.Data.StorageQuotaGB != nil && *c.Data.StorageQuotaGB > 0

	_, err = validator.ValidateTarget(op, c.Data, false)
	if err != nil {
		op.Errorf("Configuring cannot continue - target validation failed: %s", err)
		return errors.New("configure failed")
	}
	executor := management.NewDispatcher(op, validator.Session(), management.ActionConfigure, c.Force)

	var vch *vm.VirtualMachine
	if c.Data.ID != "" {
		vch, err = executor.NewVCHFromID(c.Data.ID)
	} else {
		vch, err = executor.NewVCHFromComputePath(c.Data.ComputeResourcePath, c.Data.DisplayName, validator)
	}
	if err != nil {
		op.Errorf("Failed to get Virtual Container Host %s", c.DisplayName)
		op.Error(err)
		return errors.New("configure failed")
	}

	op.Info("")
	op.Infof("VCH ID: %s", vch.Reference().String())

	if c.ResetInProgressFlag {
		if err = vch.SetVCHUpdateStatus(op, false); err != nil {
			op.Error("Failed to reset UpdateInProgress flag")
			op.Error(err)
			return errors.New("configure failed")
		}
		op.Info("Reset UpdateInProgress flag successfully")
		return nil
	}

	vchConfig, err := executor.GetVCHConfig(vch)
	if err != nil {
		op.Error("Failed to get Virtual Container Host configuration")
		op.Error(err)
		return errors.New("configure failed")
	}

	installerVer := version.GetBuild().PluginVersion
	if vchConfig.ExecutorConfig.Version == nil {
		op.Error("Cannot configure VCH with version unavailable")
		return errors.New("configure failed")
	}
	if vchConfig.ExecutorConfig.Version.PluginVersion < installerVer {
		op.Errorf("Cannot configure VCH with version %s, please upgrade first", vchConfig.ExecutorConfig.Version.ShortVersion())
		return errors.New("configure failed")
	}

	// Convert guestinfo *VirtualContainerHost back to *Data, decrypt secret data
	oldData, err := validate.NewDataFromConfig(op, validator.Session().Finder, vchConfig)
	if err != nil {
		op.Error("Configuring cannot continue: configuration conversion failed")
		op.Error(err)
		return err
	}

	if err = validator.SetDataFromVM(op, vch, oldData); err != nil {
		op.Error("Configuring cannot continue: querying configuration from VM failed")
		op.Error(err)
		return err
	}

	// Handle the three options for the --affinity-vm-group flag: unset, true, false.
	//
	// If the user hasn't specified the flag, we don't want to make a change. If they have
	// specified it and are requesting a change, track that.
	if clic.IsSet("affinity-vm-group") {
		if !oldData.UseVMGroup && c.Data.UseVMGroup {
			oldData.CreateVMGroup = true
		}

		if oldData.UseVMGroup && !c.Data.UseVMGroup {
			oldData.DeleteVMGroup = true
		}

		oldData.UseVMGroup = c.Data.UseVMGroup
	}

	// using new configuration override configuration query from guestinfo
	if err = oldData.CopyNonEmpty(c.Data); err != nil {
		op.Error("Configuring cannot continue: copying configuration failed")
		return err
	}
	// Copy original and merged resources
	// This copy is needed so that we validate only the resource settings
	// (--mem, --cpu, etc) supplied by the user.
	inputResources := c.Data.ResourceLimits
	mergedResources := oldData.ResourceLimits

	// overwriting user input w/merged dataset
	c.Data = oldData

	// Set the ResourceLimits to the input received from
	// the user
	oldData.ResourceLimits = inputResources

	// in Create we process certificates as part of processParams but we need the old conf
	// to do this in the context of Configure so we need to call this method here instead
	if err = c.processCertificates(op, c.Data.ClientNetwork, c.Data.PublicNetwork, c.Data.ManagementNetwork); err != nil {
		return err
	}

	// evaluate merged configuration
	newConfig, err := validator.Validate(op, c.Data, false)
	if err != nil {
		op.Error("Configuring cannot continue: configuration validation failed")
		return err
	}

	if updatedStorageQuota {
		_, err = validator.ValidateStorageQuota(op, *c.StorageQuotaGB, vchConfig, vch)
		if err != nil {
			op.Error("Configuring cannot continue: storage quota validation failed")
			return err
		}
	}

	// The user supplied resource information has been validated, so
	// switch back to the merged results
	c.Data.ResourceLimits = mergedResources

	if c.StorageQuotaGB != nil {
		// Treat minus values as unlimited
		if *c.StorageQuotaGB <= 0 {
			vchConfig.StorageQuota = 0
		} else {
			vchConfig.StorageQuota = int64(*c.StorageQuotaGB) * units.GiB
		}
	}

	if c.ContainerCount != nil {
		// Treat minus values as unlimited
		if *c.ContainerCount <= 0 {
			vchConfig.ContainerCount = 0
		} else {
			vchConfig.ContainerCount = *c.ContainerCount
		}
	}

	// TODO: copy changed configuration here. https://github.com/vmware/vic/issues/2911
	c.copyChangedConf(vchConfig, newConfig, clic, validator)

	vConfig := validator.AddDeprecatedFields(op, vchConfig, c.Data)
	vConfig.Timeout = c.Timeout
	vConfig.VCHSizeIsSet = c.ResourceLimits.IsSet
	vConfig.CreateVMGroup = c.CreateVMGroup
	vConfig.DeleteVMGroup = c.DeleteVMGroup

	updating, err := vch.VCHUpdateStatus(op)
	if err != nil {
		op.Error("Unable to determine if upgrade/configure is in progress")
		op.Error(err)
		return errors.New("configure failed")
	}
	if updating {
		op.Error("Configure failed: another upgrade/configure operation is in progress")
		op.Error("If no other upgrade/configure process is running, use --reset-progress to reset the VCH upgrade/configure status")
		return errors.New("configure failed")
	}

	if err = vch.SetVCHUpdateStatus(op, true); err != nil {
		op.Error("Failed to set UpdateInProgress flag to true")
		op.Error(err)
		return errors.New("configure failed")
	}

	defer func() {
		if err = vch.SetVCHUpdateStatus(op, false); err != nil {
			op.Error("Failed to reset UpdateInProgress")
			op.Error(err)
		}
	}()

	if !c.Data.Rollback {
		err = executor.Configure(vchConfig, vConfig)
	} else {
		executor.Action = management.ActionRollback
		err = executor.Rollback(vchConfig, vConfig)
	}

	if err != nil {
		// configure failed
		executor.CollectDiagnosticLogs()
		return errors.New("configure failed")
	}

	op.Info("Completed successfully")

	return nil
}
