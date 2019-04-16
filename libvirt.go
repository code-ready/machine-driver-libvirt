package libvirt

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	libvirt "github.com/libvirt/libvirt-go"

	// Machine-drivers
	"github.com/code-ready/machine/libmachine/drivers"
	"github.com/code-ready/machine/libmachine/log"
	"github.com/code-ready/machine/libmachine/mcnflag"
	"github.com/code-ready/machine/libmachine/state"

	// CRC system bundle
	"github.com/code-ready/crc/pkg/crc/machine/bundle"
)

type Driver struct {
	*drivers.BaseDriver

	// CRC System bundle
    BundlePath       string

    // Driver specific configuration
	Memory           int
	CPU              int
	Network          string
	PrivateNetwork   string
	DiskPath         string
	CacheMode        string
	IOMode           string

	// Libvirt connection and state
	connectionString string
	conn             *libvirt.Connect
	VM               *libvirt.Domain
	vmLoaded         bool
}

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.IntFlag{
			Name:  "kvm-memory",
			Usage: "Size of memory for host in MB",
			Value: 1024,
		},
		mcnflag.IntFlag{
			Name:  "kvm-disk-size",
			Usage: "Size of disk for host in MB",
			Value: 20000,
		},
		mcnflag.IntFlag{
			Name:  "kvm-cpu-count",
			Usage: "Number of CPUs",
			Value: 1,
		},
		// TODO - support for multiple networks
		mcnflag.StringFlag{
			Name:  "kvm-network",
			Usage: "Name of network to connect to",
			Value: "default",
		},
		mcnflag.StringFlag{
			Name:  "kvm-cache-mode",
			Usage: "Disk cache mode: default, none, writethrough, writeback, directsync, or unsafe",
			Value: "default",
		},
		mcnflag.StringFlag{
			Name:  "kvm-io-mode",
			Usage: "Disk IO mode: threads, native",
			Value: "threads",
		},
		mcnflag.StringFlag{
			EnvVar: "KVM_SSH_USER",
			Name:   "kvm-ssh-user",
			Usage:  "SSH username",
			Value:  defaultSSHUser,
		},
		/* Not yet implemented
		mcnflag.Flag{
			Name:  "kvm-no-share",
			Usage: "Disable the mount of your home directory",
		},
		*/
	}
}

func (d *Driver) GetMachineName() string {
	return d.MachineName
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetSSHKeyPath() string {
	return "" //d.ResolveStorePath("id_rsa")
}

func (d *Driver) GetSSHPort() (int, error) {
	if d.SSHPort == 0 {
		d.SSHPort = 22
	}

	return d.SSHPort, nil
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = "docker"
	}

	return d.SSHUser
}

func (d *Driver) DriverName() string {
	return DriverName
}

func (d *Driver) DriverVersion() string {
	return DriverVersion
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	log.Debugf("SetConfigFromFlags called")
	d.Memory = flags.Int("libvirt-memory")
	d.CPU = flags.Int("libvirt-cpu-count")
	d.Network = flags.String("libvirt-network")
	d.CacheMode = flags.String("libvirt-cache-mode")
	d.IOMode = flags.String("libvirt-io-mode")
	d.SSHPort = 22
	d.DiskPath = d.ResolveStorePath(fmt.Sprintf("%s.img", d.MachineName))

	// CRC system bundle
	d.BundlePath = flags.String("libvirt-bundlepath")
	return nil
}

func (d *Driver) GetURL() (string, error) {
	return "", nil
}

func (d *Driver) getConn() (*libvirt.Connect, error) {
	if d.conn == nil {
		conn, err := libvirt.NewConnect(connectionString)
		if err != nil {
			log.Errorf("Failed to connect to libvirt: %s", err)
			return &libvirt.Connect{}, errors.New("Unable to connect to kvm driver, did you add yourself to the libvirtd group?")
		}
		d.conn = conn
	}
	return d.conn, nil
}

// Create, or verify the private network is properly configured
func (d *Driver) validatePrivateNetwork() error {
	log.Debug("Validating private network")
	conn, err := d.getConn()
	if err != nil {
		return err
	}
	network, err := conn.LookupNetworkByName(d.PrivateNetwork)
	if err == nil {
		xmldoc, err := network.GetXMLDesc(0)
		if err != nil {
			return err
		}
		/* XML structure:
		<network>
		    ...
		    <ip address='a.b.c.d' netmask='255.255.255.0'>
		        <dhcp>
		            <range start='a.b.c.d' end='w.x.y.z'/>
		        </dhcp>
		*/
		type Ip struct {
			Address string `xml:"address,attr"`
			Netmask string `xml:"netmask,attr"`
		}
		type Network struct {
			Ip Ip `xml:"ip"`
		}

		var nw Network
		err = xml.Unmarshal([]byte(xmldoc), &nw)
		if err != nil {
			return err
		}

		if nw.Ip.Address == "" {
			return fmt.Errorf("%s network doesn't have DHCP configured properly", d.PrivateNetwork)
		}
		// Corner case, but might happen...
		if active, err := network.IsActive(); !active {
			log.Debugf("Reactivating private network: %s", err)
			err = network.Create()
			if err != nil {
				log.Warnf("Failed to Start network: %s", err)
				return err
			}
		}
		return nil
	}
	// TODO - try a couple pre-defined networks and look for conflicts before
	//        settling on one
	xml := fmt.Sprintf(networkXML, d.PrivateNetwork,
		"192.168.42.1",
		"255.255.255.0",
		"192.168.42.2",
		"192.168.42.254")

	network, err = conn.NetworkDefineXML(xml)
	if err != nil {
		log.Errorf("Failed to create private network: %s", err)
		return nil
	}
	err = network.SetAutostart(true)
	if err != nil {
		log.Warnf("Failed to set private network to autostart: %s", err)
	}
	err = network.Create()
	if err != nil {
		log.Warnf("Failed to Start network: %s", err)
		return err
	}
	return nil
}

func (d *Driver) validateNetwork(name string) error {
	log.Debugf("Validating network %s", name)
	conn, err := d.getConn()
	if err != nil {
		return err
	}
	_, err = conn.LookupNetworkByName(name)
	if err != nil {
		log.Errorf("Unable to locate network %s", name)
		return err
	}
	return nil
}

func (d *Driver) PreCreateCheck() error {
	conn, err := d.getConn()
	if err != nil {
		return err
	}

	// TODO We could look at conn.GetCapabilities()
	// parse the XML, and look for kvm
	log.Debug("About to check libvirt version")

	// TODO might want to check minimum version
	_, err = conn.GetLibVersion()
	if err != nil {
		log.Warnf("Unable to get libvirt version")
		return err
	}
	err = d.validatePrivateNetwork()
	if err != nil {
		return err
	}
	err = d.validateNetwork(d.Network)
	if err != nil {
		return err
	}
	// Others...?
	return nil
}

func (d *Driver) Create() error {
	if err := os.MkdirAll(d.ResolveStorePath("."), 0755); err != nil {
		return err
	}

	// Libvirt typically runs as a deprivileged service account and
	// needs the execute bit set for directories that contain disks
	for dir := d.ResolveStorePath("."); dir != "/"; dir = filepath.Dir(dir) {
		log.Debugf("Verifying executable bit set on %s", dir)
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		mode := info.Mode()
		if mode&0001 != 1 {
			log.Debugf("Setting executable bit set on %s", dir)
			mode |= 0001
			os.Chmod(dir, mode)
		}
	}

	log.Debugf("Extracting system bundle...")
	err := bundle.Extract(d.BundlePath, d.ResolveStorePath("."))
	if err != nil {
		return err
	}

	// use disk image instead

	log.Debugf("Defining VM...")
	tmpl, err := template.New("domain").Parse(domainXMLTemplate)
	if err != nil {
		return err
	}
	var xml bytes.Buffer
	err = tmpl.Execute(&xml, d)
	if err != nil {
		return err
	}

	conn, err := d.getConn()
	if err != nil {
		return err
	}
	vm, err := conn.DomainDefineXML(xml.String())
	if err != nil {
		log.Warnf("Failed to create the VM: %s", err)
		return err
	}
	d.VM = vm
	d.vmLoaded = true

	return d.Start()
}

func (d *Driver) Start() error {
	log.Debugf("Starting VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	if err := d.VM.Create(); err != nil {
		log.Warnf("Failed to start: %s", err)
		return err
	}

	// They wont start immediately
	time.Sleep(5 * time.Second)

	for i := 0; i < 90; i++ {
		time.Sleep(time.Second)
		ip, _ := d.GetIP()
		if ip != "" {
			// Add a second to let things settle
			time.Sleep(time.Second)
			return nil
		}
		log.Debugf("Waiting for the VM to come up... %d", i)
	}
	log.Warnf("Unable to determine VM's IP address, did it fail to boot?")
	return nil
}

func (d *Driver) Stop() error {
	log.Debugf("Stopping VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	s, err := d.GetState()
	if err != nil {
		return err
	}

	if s != state.Stopped {
		err := d.VM.Shutdown()
		if err != nil {
			log.Warnf("Failed to gracefully shutdown VM")
			return err
		}
		for i := 0; i < 90; i++ {
			time.Sleep(time.Second)
			s, _ := d.GetState()
			log.Debugf("VM state: %s", s)
			if s == state.Stopped {
				return nil
			}
		}
		return errors.New("VM Failed to gracefully shutdown, try the kill command")
	}
	return nil
}

func (d *Driver) Remove() error {
	log.Debugf("Removing VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	// Note: If we switch to qcow disks instead of raw the user
	//       could take a snapshot.  If you do, then Undefine
	//       will fail unless we nuke the snapshots first
	d.VM.Destroy() // Ignore errors
	return d.VM.Undefine()
}

func (d *Driver) Restart() error {
	log.Debugf("Restarting VM %s", d.MachineName)
	if err := d.Stop(); err != nil {
		return err
	}
	return d.Start()
}

func (d *Driver) Kill() error {
	log.Debugf("Killing VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	return d.VM.Destroy()
}

func (d *Driver) GetState() (state.State, error) {
	log.Debugf("Getting current state...")
	if err := d.validateVMRef(); err != nil {
		return state.None, err
	}
	virState, _, err := d.VM.GetState()
	if err != nil {
		return state.None, err
	}
	switch virState {
	case libvirt.DOMAIN_NOSTATE:
		return state.None, nil
	case libvirt.DOMAIN_RUNNING:
		return state.Running, nil
	case libvirt.DOMAIN_BLOCKED:
		// TODO - Not really correct, but does it matter?
		return state.Error, nil
	case libvirt.DOMAIN_PAUSED:
		return state.Paused, nil
	case libvirt.DOMAIN_SHUTDOWN:
		return state.Stopped, nil
	case libvirt.DOMAIN_CRASHED:
		return state.Error, nil
	case libvirt.DOMAIN_PMSUSPENDED:
		return state.Saved, nil
	case libvirt.DOMAIN_SHUTOFF:
		return state.Stopped, nil
	}
	return state.None, nil
}

func (d *Driver) validateVMRef() error {
	if !d.vmLoaded {
		log.Debugf("Fetching VM...")
		conn, err := d.getConn()
		if err != nil {
			return err
		}
		vm, err := conn.LookupDomainByName(d.MachineName)
		if err != nil {
			log.Warnf("Failed to fetch machine")
		} else {
			d.VM = vm
			d.vmLoaded = true
		}
	}
	return nil
}

// This implementation is specific to default networking in libvirt
// with dnsmasq
func (d *Driver) getMAC() (string, error) {
	if err := d.validateVMRef(); err != nil {
		return "", err
	}
	xmldoc, err := d.VM.GetXMLDesc(0)
	if err != nil {
		return "", err
	}
	/* XML structure:
	<domain>
	    ...
	    <devices>
	        ...
	        <interface type='network'>
	            ...
	            <mac address='52:54:00:d2:3f:ba'/>
	            ...
	        </interface>
	        ...
	*/
	type Mac struct {
		Address string `xml:"address,attr"`
	}
	type Source struct {
		Network string `xml:"network,attr"`
	}
	type Interface struct {
		Type   string `xml:"type,attr"`
		Mac    Mac    `xml:"mac"`
		Source Source `xml:"source"`
	}
	type Devices struct {
		Interfaces []Interface `xml:"interface"`
	}
	type Domain struct {
		Devices Devices `xml:"devices"`
	}

	var dom Domain
	err = xml.Unmarshal([]byte(xmldoc), &dom)
	if err != nil {
		return "", err
	}
	// Always assume the second interface is the one we want
	if len(dom.Devices.Interfaces) < 2 {
		return "", fmt.Errorf("VM doesn't have enough network interfaces.  Expected at least 2, found %d",
			len(dom.Devices.Interfaces))
	}
	return dom.Devices.Interfaces[1].Mac.Address, nil
}

func (d *Driver) getIPByMACFromLeaseFile(mac string) (string, error) {
	leaseFile := fmt.Sprintf(dnsmasqLeases, d.PrivateNetwork)
	data, err := ioutil.ReadFile(leaseFile)
	if err != nil {
		log.Debugf("Failed to retrieve dnsmasq leases from %s", leaseFile)
		return "", err
	}
	for lineNum, line := range strings.Split(string(data), "\n") {
		if len(line) == 0 {
			continue
		}
		entries := strings.Split(line, " ")
		if len(entries) < 3 {
			log.Warnf("Malformed dnsmasq line %d", lineNum+1)
			return "", errors.New("Malformed dnsmasq file")
		}
		if strings.ToLower(entries[1]) == strings.ToLower(mac) {
			log.Debugf("IP address: %s", entries[2])
			return entries[2], nil
		}
	}
	return "", nil
}

func (d *Driver) getIPByMacFromSettings(mac string) (string, error) {
	conn, err := d.getConn()
	if err != nil {
		return "", err
	}
	network, err := conn.LookupNetworkByName(d.PrivateNetwork)
	if err != nil {
		log.Warnf("Failed to find network: %s", err)
		return "", err
	}
	bridge_name, err := network.GetBridgeName()
	if err != nil {
		log.Warnf("Failed to get network bridge: %s", err)
		return "", err
	}
	statusFile := fmt.Sprintf(dnsmasqStatus, bridge_name)
	data, err := ioutil.ReadFile(statusFile)
	type Lease struct {
		Ip_address  string `json:"ip-address"`
		Mac_address string `json:"mac-address"`
		// Other unused fields omitted
	}
	var s []Lease

	err = json.Unmarshal(data, &s)
	if err != nil {
		log.Warnf("Failed to decode dnsmasq lease status: %s", err)
		return "", err
	}
	ipAddr := ""
	for _, value := range s {
		if strings.ToLower(value.Mac_address) == strings.ToLower(mac) {
			// If there are multiple entries,
			// the last one is the most current
			ipAddr = value.Ip_address
		}
	}
	if ipAddr != "" {
		log.Debugf("IP address: %s", ipAddr)
	}
	return ipAddr, nil
}

func (d *Driver) GetIP() (string, error) {
	log.Debugf("GetIP called for %s", d.MachineName)
	mac, err := d.getMAC()
	if err != nil {
		return "", err
	}
	/*
	 * TODO - Figure out what version of libvirt changed behavior and
	 *        be smarter about selecting which algorithm to use
	 */
	ip, err := d.getIPByMACFromLeaseFile(mac)
	if ip == "" {
		ip, err = d.getIPByMacFromSettings(mac)
	}
	log.Debugf("Unable to locate IP address for MAC %s", mac)
	return ip, err
}

func NewDriver(hostName, storePath string) drivers.Driver {
	return &Driver{
		PrivateNetwork: privateNetworkName,
		BaseDriver: &drivers.BaseDriver{
			SSHUser:     defaultSSHUser,
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}