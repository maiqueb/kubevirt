/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=generated_mock_$GOFILE

package network

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	netutils "k8s.io/utils/net"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/log"
	"kubevirt.io/client-go/precond"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var bridgeFakeIP = "169.254.75.1%d/32"

type BindMechanism interface {
	discoverPodNetworkInterface() error
	preparePodNetworkInterfaces() error

	hasCachedInterface() bool
	loadCachedInterface(pid string) error
	setCachedInterface(pid string) error

	// virt-handler that executes phase1 of network configuration needs to
	// pass details about discovered networking port into phase2 that is
	// executed by virt-launcher. Virt-launcher cannot discover some of
	// these details itself because at this point phase1 is complete and
	// ports are rewired, meaning, routes and IP addresses configured by
	// CNI plugin may be gone. For this matter, we use a cached VIF file to
	// pass discovered information between phases.
	loadCachedVIF(pid string) error
	setCachedVIF(pid string) error

	// The following entry points require domain initialized for the
	// binding and can be used in phase2 only.
	decorateConfig() error
	startDHCP(vmi *v1.VirtualMachineInstance) error
}

type podNICImpl struct{}

func getVifFilePath(pid, name string) string {
	return fmt.Sprintf(vifCacheFile, pid, name)
}

func writeVifFile(buf []byte, pid, name string) error {
	err := ioutil.WriteFile(getVifFilePath(pid, name), buf, 0644)
	if err != nil {
		return fmt.Errorf("error writing vif object: %v", err)
	}
	return nil
}

func setPodInterfaceCache(iface *v1.Interface, podInterfaceName string, uid string) error {
	cache := PodCacheInterface{Iface: iface}

	ipv4, ipv6, err := readIPAddressesFromLink(podInterfaceName)
	if err != nil {
		return err
	}

	switch {
	case ipv4 != "" && ipv6 != "":
		cache.PodIPs, err = sortIPsBasedOnPrimaryIP(ipv4, ipv6)
		if err != nil {
			return err
		}
	case ipv4 != "":
		cache.PodIPs = []string{ipv4}
	case ipv6 != "":
		cache.PodIPs = []string{ipv6}
	default:
		return nil
	}

	cache.PodIP = cache.PodIPs[0]
	err = WriteToVirtHandlerCachedFile(cache, types.UID(uid), iface.Name)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to write pod Interface to cache, %s", err.Error())
		return err
	}

	return nil
}

func readIPAddressesFromLink(podInterfaceName string) (string, string, error) {
	link, err := Handler.LinkByName(podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", podInterfaceName)
		return "", "", err
	}

	// get IP address
	addrList, err := Handler.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a address for interface: %s", podInterfaceName)
		return "", "", err
	}

	// no ip assigned. ipam disabled
	if len(addrList) == 0 {
		return "", "", nil
	}

	var ipv4, ipv6 string
	for _, addr := range addrList {
		if addr.IP.IsGlobalUnicast() {
			if netutils.IsIPv6(addr.IP) && ipv6 == "" {
				ipv6 = addr.IP.String()
			} else if !netutils.IsIPv6(addr.IP) && ipv4 == "" {
				ipv4 = addr.IP.String()
			}
		}
	}

	return ipv4, ipv6, nil
}

// sortIPsBasedOnPrimaryIP returns a sorted slice of IP/s based on the detected cluster primary IP.
// The operation clones the Pod status IP list order logic.
func sortIPsBasedOnPrimaryIP(ipv4, ipv6 string) ([]string, error) {
	ipv4Primary, err := Handler.IsIpv4Primary()
	if err != nil {
		return nil, err
	}

	if ipv4Primary {
		return []string{ipv4, ipv6}, nil
	}

	return []string{ipv6, ipv4}, nil
}

func (l *podNICImpl) PlugPhase1(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, podInterfaceName string, pid int) error {
	initHandler()

	// There is nothing to plug for SR-IOV devices
	if iface.SRIOV != nil {
		return nil
	}

	bindMechanism, err := getPhase1Binding(vmi, iface, network, podInterfaceName, pid)
	if err != nil {
		return err
	}

	pidStr := fmt.Sprintf("%d", pid)
	if err := bindMechanism.loadCachedInterface(pidStr); err != nil {
		return err
	}

	doesExist := bindMechanism.hasCachedInterface()
	// ignore the bindMechanism.loadCachedInterface for slirp and set the Pod interface cache
	if !doesExist || iface.Slirp != nil {
		err := setPodInterfaceCache(iface, podInterfaceName, string(vmi.ObjectMeta.UID))
		if err != nil {
			return err
		}
	}
	if !doesExist {
		err = bindMechanism.discoverPodNetworkInterface()
		if err != nil {
			return err
		}

		if err := bindMechanism.preparePodNetworkInterfaces(); err != nil {
			log.Log.Reason(err).Error("failed to prepare pod networking")
			return createCriticalNetworkError(err)
		}

		err = bindMechanism.setCachedInterface(fmt.Sprintf("%d", pid))
		if err != nil {
			log.Log.Reason(err).Error("failed to save interface configuration")
			return createCriticalNetworkError(err)
		}

		err = bindMechanism.setCachedVIF(fmt.Sprintf("%d", pid))
		if err != nil {
			log.Log.Reason(err).Error("failed to save vif configuration")
			return createCriticalNetworkError(err)
		}
	}

	return nil
}

func createCriticalNetworkError(err error) *CriticalNetworkError {
	return &CriticalNetworkError{fmt.Sprintf("Critical network error: %v", err)}
}

func ensureDHCP(vmi *v1.VirtualMachineInstance, bindMechanism BindMechanism, podInterfaceName string) error {
	dhcpStartedFile := fmt.Sprintf("/var/run/kubevirt-private/dhcp_started-%s", podInterfaceName)
	_, err := os.Stat(dhcpStartedFile)
	if os.IsNotExist(err) {
		if err := bindMechanism.startDHCP(vmi); err != nil {
			return fmt.Errorf("failed to start DHCP server for interface %s", podInterfaceName)
		}
		newFile, err := os.Create(dhcpStartedFile)
		if err != nil {
			return fmt.Errorf("failed to create dhcp started file %s: %s", dhcpStartedFile, err)
		}
		newFile.Close()
	}
	return nil
}

func (l *podNICImpl) PlugPhase2(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, domain *api.Domain, podInterfaceName string) error {
	precond.MustNotBeNil(domain)
	initHandler()

	// There is nothing to plug for SR-IOV devices
	if iface.SRIOV != nil {
		return nil
	}

	bindMechanism, err := getPhase2Binding(vmi, iface, network, domain, podInterfaceName)
	if err != nil {
		return err
	}

	pid := "self"

	if err := bindMechanism.loadCachedInterface(pid); err != nil {
		log.Log.Reason(err).Critical("failed to load cached interface configuration")
	}
	if !bindMechanism.hasCachedInterface() {
		log.Log.Reason(err).Critical("cached interface configuration doesn't exist")
	}

	if err = bindMechanism.loadCachedVIF(pid); err != nil {
		log.Log.Reason(err).Critical("failed to load cached vif configuration")
	}

	err = bindMechanism.decorateConfig()
	if err != nil {
		log.Log.Reason(err).Critical("failed to create libvirt configuration")
	}

	err = ensureDHCP(vmi, bindMechanism, podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Criticalf("failed to ensure dhcp service running for %s: %s", podInterfaceName, err)
		panic(err)
	}

	return nil
}

func retrieveMacAddress(iface *v1.Interface) (*net.HardwareAddr, error) {
	if iface.MacAddress != "" {
		macAddress, err := net.ParseMAC(iface.MacAddress)
		if err != nil {
			return nil, err
		}
		return &macAddress, nil
	}
	return nil, nil
}

func getPhase1Binding(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, podInterfaceName string, launcherPID int) (BindMechanism, error) {
	if iface.Bridge != nil {
		return newBridgeBindingMechPhase1(vmi, iface, podInterfaceName, launcherPID)
	}
	if iface.Masquerade != nil {
		return newMasqueradeBindingMechPhase1(vmi, iface, network, podInterfaceName, launcherPID)
	}
	if iface.Slirp != nil {
		return &SlirpBindMechanism{vmi: vmi, iface: iface, launcherPID: launcherPID}, nil
	}
	if iface.Macvtap != nil {
		return newMacvtapBindingMechPhase1(vmi, iface, podInterfaceName, launcherPID)
	}
	return nil, fmt.Errorf("Not implemented")
}

func getPhase2Binding(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, domain *api.Domain, podInterfaceName string) (BindMechanism, error) {
	if iface.Bridge != nil {
		return newBridgeBindingMechPhase2(vmi, iface, podInterfaceName, domain)
	}
	if iface.Masquerade != nil {
		return newMasqueradeBindingMechPhase2(vmi, iface, network, domain, podInterfaceName)
	}
	if iface.Slirp != nil {
		return &SlirpBindMechanism{vmi: vmi, iface: iface, domain: domain}, nil
	}
	if iface.Macvtap != nil {
		return newMacvtapBindingMechPhase2(vmi, iface, domain, podInterfaceName)
	}
	return nil, fmt.Errorf("Not implemented")
}

func newMacvtapBindingMechPhase1(vmi *v1.VirtualMachineInstance, iface *v1.Interface, podInterfaceName string, launcherPID int) (*MacvtapBindMechanism, error) {
	macvtapBindMech, err := newMacvtapBindingMechPhase2(vmi, iface, nil, podInterfaceName)
	if err != nil {
		return nil, err
	}
	macvtapBindMech.launcherPID = launcherPID
	return macvtapBindMech, nil
}

func newMacvtapBindingMechPhase2(vmi *v1.VirtualMachineInstance, iface *v1.Interface, domain *api.Domain, podInterfaceName string) (*MacvtapBindMechanism, error) {
	mac, err := retrieveMacAddress(iface)
	if err != nil {
		return nil, err
	}

	return &MacvtapBindMechanism{
		vmi:              vmi,
		iface:            iface,
		domain:           domain,
		podInterfaceName: podInterfaceName,
		mac:              mac,
	}, nil
}

func newMasqueradeBindingMechPhase1(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, podInterfaceName string, launcherPID int) (*MasqueradeBindMechanism, error) {
	masqueradeBindMech, err := newMasqueradeBindingMechPhase2(vmi, iface, network, nil, podInterfaceName)
	if err != nil {
		return nil, err
	}

	isMultiqueue := (vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue != nil) && (*vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue)
	if isMultiqueue {
		masqueradeBindMech.queueNumber = converter.CalculateNetworkQueues(vmi)
	}

	masqueradeBindMech.launcherPID = launcherPID
	return masqueradeBindMech, nil
}

func newMasqueradeBindingMechPhase2(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, domain *api.Domain, podInterfaceName string) (*MasqueradeBindMechanism, error) {
	vif, err := newVIF(iface, podInterfaceName)
	if err != nil {
		return nil, err
	}
	return &MasqueradeBindMechanism{iface: iface,
		vmi:                 vmi,
		vif:                 vif,
		domain:              domain,
		podInterfaceName:    podInterfaceName,
		vmNetworkCIDR:       network.Pod.VMNetworkCIDR,
		vmIpv6NetworkCIDR:   "", // TODO add ipv6 cidr to PodNetwork schema
		bridgeInterfaceName: fmt.Sprintf("k6t-%s", podInterfaceName)}, nil
}

func newBridgeBindingMechPhase1(vmi *v1.VirtualMachineInstance, iface *v1.Interface, podInterfaceName string, launcherPID int) (*BridgeBindMechanism, error) {
	bridgeBindMech, err := newBridgeBindingMechPhase2(vmi, iface, podInterfaceName, nil)
	if err != nil {
		return nil, err
	}

	isMultiqueue := (vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue != nil) && (*vmi.Spec.Domain.Devices.NetworkInterfaceMultiQueue)
	if isMultiqueue {
		bridgeBindMech.queueNumber = converter.CalculateNetworkQueues(vmi)
	}

	bridgeBindMech.launcherPID = launcherPID
	return bridgeBindMech, nil
}

func newBridgeBindingMechPhase2(vmi *v1.VirtualMachineInstance, iface *v1.Interface, podInterfaceName string, domain *api.Domain) (*BridgeBindMechanism, error) {
	vif, err := newVIF(iface, podInterfaceName)
	if err != nil {
		return nil, err
	}
	return &BridgeBindMechanism{iface: iface,
		vmi:                 vmi,
		vif:                 vif,
		domain:              domain,
		podInterfaceName:    podInterfaceName,
		bridgeInterfaceName: fmt.Sprintf("k6t-%s", podInterfaceName)}, nil
}

func setCachedVIF(vif VIF, launcherPID string, ifaceName string) error {
	buf, err := json.MarshalIndent(vif, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling vif object: %v", err)
	}

	return writeVifFile(buf, launcherPID, ifaceName)
}

type BridgeBindMechanism struct {
	vmi                 *v1.VirtualMachineInstance
	vif                 *VIF
	iface               *v1.Interface
	virtIface           *api.Interface
	podNicLink          netlink.Link
	domain              *api.Domain
	podInterfaceName    string
	bridgeInterfaceName string
	arpIgnore           bool
	launcherPID         int
	queueNumber         uint32
}

func (b *BridgeBindMechanism) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", b.podInterfaceName)
		return err
	}
	b.podNicLink = link

	// get IP address
	addrList, err := Handler.AddrList(b.podNicLink, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get an ip address for %s", b.podInterfaceName)
		return err
	}
	if len(addrList) == 0 {
		b.vif.IPAMDisabled = true
	} else {
		b.vif.IP = addrList[0]
		b.vif.IPAMDisabled = false
	}

	if len(b.vif.MAC) == 0 {
		// Get interface MAC address
		mac, err := Handler.GetMacDetails(b.podInterfaceName)
		if err != nil {
			log.Log.Reason(err).Errorf("failed to get MAC for %s", b.podInterfaceName)
			return err
		}
		b.vif.MAC = mac
	}

	if b.podNicLink.Attrs().MTU < 0 || b.podNicLink.Attrs().MTU > 65535 {
		return fmt.Errorf("MTU value out of range ")
	}

	// Get interface MTU
	b.vif.Mtu = uint16(b.podNicLink.Attrs().MTU)

	if !b.vif.IPAMDisabled {
		// Handle interface routes
		if err := b.setInterfaceRoutes(); err != nil {
			return err
		}
	}
	return nil
}

func (b *BridgeBindMechanism) getFakeBridgeIP() (string, error) {
	ifaces := b.vmi.Spec.Domain.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Name == b.iface.Name {
			return fmt.Sprintf(bridgeFakeIP, i), nil
		}
	}
	return "", fmt.Errorf("Failed to generate bridge fake address for interface %s", b.iface.Name)
}

func (b *BridgeBindMechanism) startDHCP(vmi *v1.VirtualMachineInstance) error {
	if !b.vif.IPAMDisabled {
		addr, err := b.getFakeBridgeIP()
		if err != nil {
			return err
		}
		fakeServerAddr, err := netlink.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("failed to parse address while starting DHCP server: %s", addr)
		}
		log.Log.Object(b.vmi).Infof("bridge pod interface: %+v %+v", b.vif, b)
		return Handler.StartDHCP(b.vif, fakeServerAddr.IP, b.bridgeInterfaceName, b.iface.DHCPOptions, true)
	}
	return nil
}

func (b *BridgeBindMechanism) preparePodNetworkInterfaces() error {
	// Set interface link to down to change its MAC address
	if err := Handler.LinkSetDown(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to bring link down for interface: %s", b.podInterfaceName)
		return err
	}

	tapDeviceName := generateTapDeviceName(b.podInterfaceName)

	if !b.vif.IPAMDisabled {
		// Remove IP from POD interface
		err := Handler.AddrDel(b.podNicLink, &b.vif.IP)

		if err != nil {
			log.Log.Reason(err).Errorf("failed to delete address for interface: %s", b.podInterfaceName)
			return err
		}

		if err := b.switchPodInterfaceWithDummy(); err != nil {
			log.Log.Reason(err).Error("failed to switch pod interface with a dummy")
			return err
		}
	}

	if _, err := Handler.SetRandomMac(b.podInterfaceName); err != nil {
		return err
	}

	if err := b.createBridge(); err != nil {
		return err
	}

	err := createAndBindTapToBridge(tapDeviceName, b.bridgeInterfaceName, b.queueNumber, b.launcherPID, int(b.vif.Mtu))
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create tap device named %s", tapDeviceName)
		return err
	}

	if b.arpIgnore {
		if err := Handler.ConfigureIpv4ArpIgnore(); err != nil {
			log.Log.Reason(err).Errorf("failed to set arp_ignore=1 on interface %s", b.bridgeInterfaceName)
			return err
		}
	}

	if err := Handler.LinkSetUp(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.podInterfaceName)
		return err
	}

	if err := Handler.LinkSetLearningOff(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to disable mac learning for interface: %s", b.podInterfaceName)
		return err
	}

	b.virtIface = &api.Interface{
		MAC: &api.MAC{MAC: b.vif.MAC.String()},
		MTU: &api.MTU{Size: strconv.Itoa(b.podNicLink.Attrs().MTU)},
		Target: &api.InterfaceTarget{
			Device:  tapDeviceName,
			Managed: "no",
		},
	}

	return nil
}

func (b *BridgeBindMechanism) decorateConfig() error {
	ifaces := b.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.GetName() == b.iface.Name {
			ifaces[i].MTU = b.virtIface.MTU
			ifaces[i].MAC = &api.MAC{MAC: b.vif.MAC.String()}
			ifaces[i].Target = b.virtIface.Target
			break
		}
	}
	return nil
}

func (b *BridgeBindMechanism) loadCachedInterface(pid string) error {
	var ifaceConfig api.Interface

	err := readFromVirtLauncherCachedFile(&ifaceConfig, pid, b.iface.Name)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	b.virtIface = &ifaceConfig
	return nil
}

func (b *BridgeBindMechanism) setCachedInterface(pid string) error {
	return writeToVirtLauncherCachedFile(b.virtIface, pid, b.iface.Name)
}

func (b *BridgeBindMechanism) hasCachedInterface() bool {
	return b.virtIface != nil
}

func (b *BridgeBindMechanism) loadCachedVIF(pid string) error {
	buf, err := ioutil.ReadFile(getVifFilePath(pid, b.iface.Name))
	if err != nil {
		return err
	}
	err = json.Unmarshal(buf, &b.vif)
	if err != nil {
		return err
	}
	b.vif.Gateway = b.vif.Gateway.To4()
	return nil
}

func (b *BridgeBindMechanism) setCachedVIF(pid string) error {
	return setCachedVIF(*b.vif, pid, b.iface.Name)
}

func (b *BridgeBindMechanism) setInterfaceRoutes() error {
	routes, err := Handler.RouteList(b.podNicLink, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get routes for %s", b.podInterfaceName)
		return err
	}
	if len(routes) == 0 {
		return fmt.Errorf("No gateway address found in routes for %s", b.podInterfaceName)
	}
	b.vif.Gateway = routes[0].Gw
	if len(routes) > 1 {
		dhcpRoutes := filterPodNetworkRoutes(routes, b.vif)
		b.vif.Routes = &dhcpRoutes
	}
	return nil
}

func (b *BridgeBindMechanism) createBridge() error {
	// Create a bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: b.bridgeInterfaceName,
		},
	}
	err := Handler.LinkAdd(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create a bridge")
		return err
	}

	err = Handler.LinkSetMaster(b.podNicLink, bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to connect interface %s to bridge %s", b.podInterfaceName, bridge.Name)
		return err
	}

	err = Handler.LinkSetUp(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.bridgeInterfaceName)
		return err
	}

	// set fake ip on a bridge
	addr, err := b.getFakeBridgeIP()
	if err != nil {
		return err
	}
	fakeaddr, err := Handler.ParseAddr(addr)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.bridgeInterfaceName)
		return err
	}

	if err := Handler.AddrAdd(bridge, fakeaddr); err != nil {
		log.Log.Reason(err).Errorf("failed to set bridge IP")
		return err
	}

	if err = Handler.DisableTXOffloadChecksum(b.bridgeInterfaceName); err != nil {
		log.Log.Reason(err).Error("failed to disable TX offload checksum on bridge interface")
		return err
	}

	return nil
}

func (b *BridgeBindMechanism) switchPodInterfaceWithDummy() error {
	originalPodInterfaceName := b.podInterfaceName
	newPodInterfaceName := fmt.Sprintf("%s-nic", originalPodInterfaceName)
	dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: originalPodInterfaceName}}

	// Set arp_ignore=1 on the bridge interface to avoid
	// the interface being seen by Duplicate Address Detection (DAD).
	// Without this, some VMs will lose their ip address after a few
	// minutes.
	b.arpIgnore = true

	// Rename pod interface to free the original name for a new dummy interface
	err := Handler.LinkSetName(b.podNicLink, newPodInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to rename interface : %s", b.podInterfaceName)
		return err
	}

	b.podInterfaceName = newPodInterfaceName
	b.podNicLink, err = Handler.LinkByName(newPodInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", b.podInterfaceName)
		return err
	}

	// Create a dummy interface named after the original interface
	err = Handler.LinkAdd(dummy)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create dummy interface : %s", originalPodInterfaceName)
		return err
	}

	// Replace original pod interface IP address to the dummy
	// Since the dummy is not connected to anything, it should not affect networking
	// Replace will add if ip doesn't exist or modify the ip
	err = Handler.AddrReplace(dummy, &b.vif.IP)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to replace original IP address to dummy interface: %s", originalPodInterfaceName)
		return err
	}

	return nil
}

type MasqueradeBindMechanism struct {
	vmi                 *v1.VirtualMachineInstance
	vif                 *VIF
	iface               *v1.Interface
	virtIface           *api.Interface
	podNicLink          netlink.Link
	domain              *api.Domain
	podInterfaceName    string
	bridgeInterfaceName string
	vmNetworkCIDR       string
	vmIpv6NetworkCIDR   string
	gatewayAddr         *netlink.Addr
	gatewayIpv6Addr     *netlink.Addr
	launcherPID         int
	queueNumber         uint32
}

func (b *MasqueradeBindMechanism) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", b.podInterfaceName)
		return err
	}
	b.podNicLink = link

	if b.podNicLink.Attrs().MTU < 0 || b.podNicLink.Attrs().MTU > 65535 {
		return fmt.Errorf("MTU value out of range ")
	}

	// Get interface MTU
	b.vif.Mtu = uint16(b.podNicLink.Attrs().MTU)

	err = configureVifV4Addresses(b, err)
	if err != nil {
		return err
	}

	ipv6Enabled, err := Handler.IsIpv6Enabled(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to verify whether ipv6 is configured on %s", b.podInterfaceName)
		return err
	}
	if ipv6Enabled {
		err = configureVifV6Addresses(b, err)
		if err != nil {
			return err
		}
	}
	return nil
}

func configureVifV4Addresses(b *MasqueradeBindMechanism, err error) error {
	if b.vmNetworkCIDR == "" {
		b.vmNetworkCIDR = api.DefaultVMCIDR
	}

	defaultGateway, vm, err := Handler.GetHostAndGwAddressesFromCIDR(b.vmNetworkCIDR)
	if err != nil {
		log.Log.Errorf("failed to get gw and vm available addresses from CIDR %s", b.vmNetworkCIDR)
		return err
	}

	gatewayAddr, err := Handler.ParseAddr(defaultGateway)
	if err != nil {
		return fmt.Errorf("failed to parse gateway ip address %s", defaultGateway)
	}
	b.vif.Gateway = gatewayAddr.IP.To4()
	b.gatewayAddr = gatewayAddr

	vmAddr, err := Handler.ParseAddr(vm)
	if err != nil {
		return fmt.Errorf("failed to parse vm ip address %s", vm)
	}
	b.vif.IP = *vmAddr
	return nil
}

func configureVifV6Addresses(b *MasqueradeBindMechanism, err error) error {
	if b.vmIpv6NetworkCIDR == "" {
		b.vmIpv6NetworkCIDR = api.DefaultVMIpv6CIDR
	}

	defaultGatewayIpv6, vmIpv6, err := Handler.GetHostAndGwAddressesFromCIDR(b.vmIpv6NetworkCIDR)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get gw and vm available ipv6 addresses from CIDR %s", b.vmIpv6NetworkCIDR)
		return err
	}

	gatewayIpv6Addr, err := Handler.ParseAddr(defaultGatewayIpv6)
	if err != nil {
		return fmt.Errorf("failed to parse gateway ipv6 address %s err %v", gatewayIpv6Addr, err)
	}
	b.vif.GatewayIpv6 = gatewayIpv6Addr.IP.To16()
	b.gatewayIpv6Addr = gatewayIpv6Addr

	vmAddr, err := Handler.ParseAddr(vmIpv6)
	if err != nil {
		return fmt.Errorf("failed to parse vm ipv6 address %s err %v", vmIpv6, err)
	}
	b.vif.IPv6 = *vmAddr
	return nil
}

func (b *MasqueradeBindMechanism) startDHCP(vmi *v1.VirtualMachineInstance) error {
	return Handler.StartDHCP(b.vif, b.vif.Gateway, b.bridgeInterfaceName, b.iface.DHCPOptions, false)
}

func (b *MasqueradeBindMechanism) preparePodNetworkInterfaces() error {
	// Create an master bridge interface
	bridgeNicName := fmt.Sprintf("%s-nic", b.bridgeInterfaceName)
	bridgeNic := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeNicName,
			MTU:  int(b.vif.Mtu),
		},
	}
	err := Handler.LinkAdd(bridgeNic)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create an interface: %s", bridgeNic.Name)
		return err
	}

	err = Handler.LinkSetUp(bridgeNic)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", bridgeNic.Name)
		return err
	}

	if err := b.createBridge(); err != nil {
		return err
	}

	tapDeviceName := generateTapDeviceName(b.podInterfaceName)
	err = createAndBindTapToBridge(tapDeviceName, b.bridgeInterfaceName, b.queueNumber, b.launcherPID, int(b.vif.Mtu))
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create tap device named %s", tapDeviceName)
		return err
	}

	err = b.createNatRules(iptables.ProtocolIPv4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create ipv4 nat rules for vm error: %v", err)
		return err
	}

	ipv6Enabled, err := Handler.IsIpv6Enabled(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to verify whether ipv6 is configured on %s", b.podInterfaceName)
		return err
	}
	if ipv6Enabled {
		err = b.createNatRules(iptables.ProtocolIPv6)
		if err != nil {
			log.Log.Reason(err).Errorf("failed to create ipv6 nat rules for vm error: %v", err)
			return err
		}
	}

	b.virtIface = &api.Interface{
		MTU: &api.MTU{Size: strconv.Itoa(b.podNicLink.Attrs().MTU)},
		Target: &api.InterfaceTarget{
			Device:  tapDeviceName,
			Managed: "no",
		},
	}
	if b.vif.MAC != nil {
		b.virtIface.MAC = &api.MAC{MAC: b.vif.MAC.String()}
	}

	return nil
}

func (b *MasqueradeBindMechanism) decorateConfig() error {
	ifaces := b.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.GetName() == b.iface.Name {
			ifaces[i].MTU = b.virtIface.MTU
			ifaces[i].MAC = b.virtIface.MAC
			ifaces[i].Target = b.virtIface.Target
			break
		}
	}
	return nil
}

func (b *MasqueradeBindMechanism) loadCachedInterface(pid string) error {
	var ifaceConfig api.Interface

	err := readFromVirtLauncherCachedFile(&ifaceConfig, pid, b.iface.Name)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	b.virtIface = &ifaceConfig
	return nil
}

func (b *MasqueradeBindMechanism) setCachedInterface(pid string) error {
	return writeToVirtLauncherCachedFile(b.virtIface, pid, b.iface.Name)
}

func (b *MasqueradeBindMechanism) hasCachedInterface() bool {
	return b.virtIface != nil
}

func (b *MasqueradeBindMechanism) loadCachedVIF(pid string) error {
	buf, err := ioutil.ReadFile(getVifFilePath(pid, b.iface.Name))
	if err != nil {
		return err
	}
	err = json.Unmarshal(buf, &b.vif)
	if err != nil {
		return err
	}
	b.vif.Gateway = b.vif.Gateway.To4()
	b.vif.GatewayIpv6 = b.vif.GatewayIpv6.To16()
	return nil
}

func (b *MasqueradeBindMechanism) setCachedVIF(pid string) error {
	return setCachedVIF(*b.vif, pid, b.iface.Name)
}

func (b *MasqueradeBindMechanism) createBridge() error {
	// Get dummy link
	bridgeNicName := fmt.Sprintf("%s-nic", b.bridgeInterfaceName)
	bridgeNicLink, err := Handler.LinkByName(bridgeNicName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to find dummy interface for bridge")
		return err
	}

	// Create a bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: b.bridgeInterfaceName,
			MTU:  int(b.vif.Mtu),
		},
	}
	err = Handler.LinkAdd(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create a bridge")
		return err
	}

	err = Handler.LinkSetMaster(bridgeNicLink, bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to connect %s interface to bridge %s", bridgeNicName, b.bridgeInterfaceName)
		return err
	}

	err = Handler.LinkSetUp(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.bridgeInterfaceName)
		return err
	}

	if err := Handler.AddrAdd(bridge, b.gatewayAddr); err != nil {
		log.Log.Reason(err).Errorf("failed to set bridge IP")
		return err
	}

	ipv6Enabled, err := Handler.IsIpv6Enabled(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to verify whether ipv6 is configured on %s", b.podInterfaceName)
		return err
	}
	if ipv6Enabled {
		if err := Handler.AddrAdd(bridge, b.gatewayIpv6Addr); err != nil {
			log.Log.Reason(err).Errorf("failed to set bridge IPv6")
			return err
		}
	}

	if err = Handler.DisableTXOffloadChecksum(b.bridgeInterfaceName); err != nil {
		log.Log.Reason(err).Error("failed to disable TX offload checksum on bridge interface")
		return err
	}

	return nil
}

func (b *MasqueradeBindMechanism) createNatRules(protocol iptables.Protocol) error {
	err := Handler.ConfigureIpForwarding(protocol)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to configure ip forwarding")
		return err
	}

	if Handler.NftablesLoad(protocol) == nil {
		return b.createNatRulesUsingNftables(protocol)
	} else if Handler.HasNatIptables(protocol) {
		return b.createNatRulesUsingIptables(protocol)
	}
	return fmt.Errorf("Couldn't configure ip nat rules")
}

func (b *MasqueradeBindMechanism) createNatRulesUsingIptables(protocol iptables.Protocol) error {
	err := Handler.IptablesNewChain(protocol, "nat", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesNewChain(protocol, "nat", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule(protocol, "nat", "POSTROUTING", "-s", b.getVifIpByProtocol(protocol), "-j", "MASQUERADE")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule(protocol, "nat", "PREROUTING", "-i", b.podInterfaceName, "-j", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule(protocol, "nat", "POSTROUTING", "-o", b.bridgeInterfaceName, "-j", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	if len(b.iface.Ports) == 0 {
		err = Handler.IptablesAppendRule(protocol, "nat", "KUBEVIRT_PREINBOUND",
			"-j",
			"DNAT",
			"--to-destination", b.getVifIpByProtocol(protocol))

		return err
	}

	for _, port := range b.iface.Ports {
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}

		err = Handler.IptablesAppendRule(protocol, "nat", "KUBEVIRT_POSTINBOUND",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"--source", getLoopbackAdrress(protocol),
			"-j",
			"SNAT",
			"--to-source", b.getGatewayByProtocol(protocol))
		if err != nil {
			return err
		}

		err = Handler.IptablesAppendRule(protocol, "nat", "KUBEVIRT_PREINBOUND",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"-j",
			"DNAT",
			"--to-destination", b.getVifIpByProtocol(protocol))
		if err != nil {
			return err
		}

		err = Handler.IptablesAppendRule(protocol, "nat", "OUTPUT",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"--destination", getLoopbackAdrress(protocol),
			"-j",
			"DNAT",
			"--to-destination", b.getVifIpByProtocol(protocol))
		if err != nil {
			return err
		}
	}

	return nil
}

func (b *MasqueradeBindMechanism) getGatewayByProtocol(proto iptables.Protocol) string {
	if proto == iptables.ProtocolIPv4 {
		return b.gatewayAddr.IP.String()
	} else {
		return b.gatewayIpv6Addr.IP.String()
	}
}

func (b *MasqueradeBindMechanism) getVifIpByProtocol(proto iptables.Protocol) string {
	if proto == iptables.ProtocolIPv4 {
		return b.vif.IP.IP.String()
	} else {
		return b.vif.IPv6.IP.String()
	}
}

func getLoopbackAdrress(proto iptables.Protocol) string {
	if proto == iptables.ProtocolIPv4 {
		return "127.0.0.1"
	} else {
		return "::1"
	}
}

func (b *MasqueradeBindMechanism) createNatRulesUsingNftables(proto iptables.Protocol) error {
	err := Handler.NftablesNewChain(proto, "nat", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesNewChain(proto, "nat", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule(proto, "nat", "postrouting", Handler.GetNFTIPString(proto), "saddr", b.getVifIpByProtocol(proto), "counter", "masquerade")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule(proto, "nat", "prerouting", "iifname", b.podInterfaceName, "counter", "jump", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule(proto, "nat", "postrouting", "oifname", b.bridgeInterfaceName, "counter", "jump", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	if len(b.iface.Ports) == 0 {
		err = Handler.NftablesAppendRule(proto, "nat", "KUBEVIRT_PREINBOUND",
			"counter", "dnat", "to", b.getVifIpByProtocol(proto))

		return err
	}

	for _, port := range b.iface.Ports {
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}

		err = Handler.NftablesAppendRule(proto, "nat", "KUBEVIRT_POSTINBOUND",
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			Handler.GetNFTIPString(proto), "saddr", getLoopbackAdrress(proto),
			"counter", "snat", "to", b.getGatewayByProtocol(proto))
		if err != nil {
			return err
		}

		err = Handler.NftablesAppendRule(proto, "nat", "KUBEVIRT_PREINBOUND",
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			"counter", "dnat", "to", b.getVifIpByProtocol(proto))
		if err != nil {
			return err
		}

		err = Handler.NftablesAppendRule(proto, "nat", "output",
			Handler.GetNFTIPString(proto), "daddr", getLoopbackAdrress(proto),
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			"counter", "dnat", "to", b.getVifIpByProtocol(proto))
		if err != nil {
			return err
		}
	}

	return nil
}

type SlirpBindMechanism struct {
	vmi         *v1.VirtualMachineInstance
	iface       *v1.Interface
	virtIface   *api.Interface
	domain      *api.Domain
	launcherPID int
}

func (s *SlirpBindMechanism) discoverPodNetworkInterface() error {
	return nil
}

func (s *SlirpBindMechanism) preparePodNetworkInterfaces() error {
	return nil
}

func (s *SlirpBindMechanism) startDHCP(vmi *v1.VirtualMachineInstance) error {
	return nil
}

func (s *SlirpBindMechanism) decorateConfig() error {
	// remove slirp interface from domain spec devices interfaces
	var foundIfaceModelType string
	ifaces := s.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.GetName() == s.iface.Name {
			s.domain.Spec.Devices.Interfaces = append(ifaces[:i], ifaces[i+1:]...)
			foundIfaceModelType = iface.Model.Type
			break
		}
	}

	if foundIfaceModelType == "" {
		return fmt.Errorf("failed to find interface %s in vmi spec", s.iface.Name)
	}

	qemuArg := fmt.Sprintf("%s,netdev=%s,id=%s", foundIfaceModelType, s.iface.Name, s.iface.Name)
	if s.iface.MacAddress != "" {
		// We assume address was already validated in API layer so just pass it to libvirt as-is.
		qemuArg += fmt.Sprintf(",mac=%s", s.iface.MacAddress)
	}
	// Add interface configuration to qemuArgs
	s.domain.Spec.QEMUCmd.QEMUArg = append(s.domain.Spec.QEMUCmd.QEMUArg, api.Arg{Value: "-device"})
	s.domain.Spec.QEMUCmd.QEMUArg = append(s.domain.Spec.QEMUCmd.QEMUArg, api.Arg{Value: qemuArg})

	return nil
}

func (s *SlirpBindMechanism) loadCachedInterface(pid string) error {
	return nil
}

func (s *SlirpBindMechanism) loadCachedVIF(pid string) error {
	return nil
}

func (b *SlirpBindMechanism) setCachedVIF(pid string) error {
	return nil
}

func (s *SlirpBindMechanism) setCachedInterface(pid string) error {
	return nil
}

func (b *SlirpBindMechanism) hasCachedInterface() bool {
	return true
}

type MacvtapBindMechanism struct {
	vmi              *v1.VirtualMachineInstance
	iface            *v1.Interface
	virtIface        *api.Interface
	domain           *api.Domain
	podInterfaceName string
	podNicLink       netlink.Link
	mac              *net.HardwareAddr
	launcherPID      int
}

func (b *MacvtapBindMechanism) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", b.podInterfaceName)
		return err
	}
	b.podNicLink = link
	b.virtIface = &api.Interface{
		MAC: &api.MAC{MAC: b.getIfaceMACAddr()},
		MTU: &api.MTU{Size: strconv.Itoa(b.podNicLink.Attrs().MTU)},
		Target: &api.InterfaceTarget{
			Device:  b.podInterfaceName,
			Managed: "no",
		},
	}

	return nil
}

func (b *MacvtapBindMechanism) getIfaceMACAddr() string {
	if b.mac != nil {
		return b.mac.String()
	} else {
		return b.podNicLink.Attrs().HardwareAddr.String()
	}
}

func (b *MacvtapBindMechanism) preparePodNetworkInterfaces() error {
	return nil
}

func (b *MacvtapBindMechanism) decorateConfig() error {
	ifaces := b.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.GetName() == b.iface.Name {
			ifaces[i].MTU = b.virtIface.MTU
			ifaces[i].MAC = b.virtIface.MAC
			ifaces[i].Target = b.virtIface.Target
			break
		}
	}
	return nil
}

func (b *MacvtapBindMechanism) loadCachedInterface(pid string) error {
	var ifaceConfig api.Interface

	err := readFromVirtLauncherCachedFile(&ifaceConfig, pid, b.iface.Name)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return err
	}

	b.virtIface = &ifaceConfig
	return nil
}

func (b *MacvtapBindMechanism) hasCachedInterface() bool {
	return b.virtIface != nil
}

func (b *MacvtapBindMechanism) setCachedInterface(pid string) error {
	return writeToVirtLauncherCachedFile(b.virtIface, pid, b.iface.Name)
}

func (b *MacvtapBindMechanism) loadCachedVIF(pid string) error {
	return nil
}

func (b *MacvtapBindMechanism) setCachedVIF(pid string) error {
	return nil
}

func (b *MacvtapBindMechanism) startDHCP(vmi *v1.VirtualMachineInstance) error {
	// macvtap will connect to the host's subnet
	return nil
}

func createAndBindTapToBridge(deviceName string, bridgeIfaceName string, queueNumber uint32, launcherPID int, mtu int) error {
	err := Handler.CreateTapDevice(deviceName, queueNumber, launcherPID, mtu)
	if err != nil {
		return err
	}
	return Handler.BindTapDeviceToBridge(deviceName, bridgeIfaceName)
}

func generateTapDeviceName(podInterfaceName string) string {
	return "tap" + podInterfaceName[3:]
}
