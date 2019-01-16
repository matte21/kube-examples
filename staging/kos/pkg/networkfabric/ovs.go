/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package networkfabric

// TODO add flows in single transaction
// TODO refactor creation of commands and flows - use templates if it helps
// TODO During creation/config ifcs, add check to make sure config is similar to what we want (need to think whether we actually need this, I think not)
// TODO add protocols at bridge creation time
// TODO consider what happens if we add an itnerface to an already existing bridge

import (
	"fmt"
	"github.com/golang/glog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	ovs     = "ovs"
	vtep    = "vtep"
	newLine = "\n"

	// time to wait after a failure before performing the needed clean up
	cleanupDelay = 1 * time.Second
)

type ovsFabric struct {
	bridge     string
	vtepOfport uint16
}

func init() {
	if f, err := NewOvSFabric("kos"); err != nil {
		panic(fmt.Sprintf("failed to create OvS network fabric: %s", err.Error()))
	} else {
		registerFabric(ovs, f)
	}
}

func NewOvSFabric(bridge string) (*ovsFabric, error) {
	f := &ovsFabric{
		bridge: bridge,
	}

	if err := f.initBridge(); err != nil {
		return nil, err
	}

	return f, nil
}

func (f *ovsFabric) initBridge() error {
	if err := f.createBridge(); err != nil {
		return err
	}

	if err := f.addVTEP(); err != nil {
		return err
	}

	vtepOfport, err := f.getIfcOfport(vtep)
	if err != nil {
		return err
	}
	f.vtepOfport = vtepOfport

	if err := f.addDefaultFlows(); err != nil {
		return err
	}

	return nil
}

func (f *ovsFabric) createBridge() error {
	createBridgeCmd := newCreateBridgeCmd(f.bridge)

	if out, err := createBridgeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create bridge \"%s\": %s: %s",
			f.bridge,
			err.Error(),
			string(out))
	}
	glog.V(4).Infof("Created OvS bridge \"%s\"", f.bridge)

	return nil
}

func (f *ovsFabric) addVTEP() error {
	addVTEPCmd := newAddVTEPCmd(f.bridge)

	if out, err := addVTEPCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add VTEP port and interface to bridge \"%s\": %s: %s",
			f.bridge,
			err.Error(),
			string(out))
	}
	glog.V(4).Infof("Added VTEP to OvS bridge \"%s\"", f.bridge)

	return nil
}

func (f *ovsFabric) addDefaultFlows() error {
	if err := f.addDefaultResubmitToT1Flow(); err != nil {
		return err
	}

	if err := f.addDefaultDropFlow(); err != nil {
		return err
	}

	return nil
}

func newCreateBridgeCmd(bridge string) *exec.Cmd {
	return exec.Command("ovs-vsctl", "--may-exist", "add-br", bridge)
}

func newAddVTEPCmd(bridge string) *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"--may-exist",
		"add-port",
		bridge,
		vtep,
		"--",
		"set",
		"interface",
		vtep,
		"type=vxlan",
		"options:key=flow",
		"options:remote_ip=flow")
}

func (f *ovsFabric) getIfcOfport(ifc string) (uint16, error) {
	getIfcOfportCmd := newGetIfcOfportCmd(ifc)

	outBytes, err := getIfcOfportCmd.CombinedOutput()
	out := strings.TrimRight(string(outBytes), newLine)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve OpenFlow port of interface %s in bridge %s: %s: %s",
			ifc,
			f.bridge,
			err.Error(),
			out)
	}

	return parseOfport(out)
}

func (f *ovsFabric) addDefaultResubmitToT1Flow() error {
	resubmitToT1 := "table=0,actions=resubmit(,1)"
	addResubmitToT1Cmd := newAddFlowToBridgeCmd(f.bridge, resubmitToT1)

	if out, err := addResubmitToT1Cmd.CombinedOutput(); err != nil {
		return newAddFlowToBridgeErr(resubmitToT1,
			f.bridge,
			err.Error(),
			strings.TrimRight(string(out), newLine))
	}
	glog.V(4).Infof("Added flow %s to OvS bridge \"%s\"", resubmitToT1, f.bridge)

	return nil
}

func (f *ovsFabric) addDefaultDropFlow() error {
	defaultDrop := "table=1,actions=drop"
	addDefaultDropCmd := newAddFlowToBridgeCmd(f.bridge, defaultDrop)

	if out, err := addDefaultDropCmd.CombinedOutput(); err != nil {
		return newAddFlowToBridgeErr(defaultDrop,
			f.bridge,
			err.Error(),
			strings.TrimRight(string(out), newLine))
	}
	glog.V(4).Infof("Added flow %s to OvS bridge \"%s\"", defaultDrop, f.bridge)

	return nil
}

func newGetIfcOfportCmd(ifc string) *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"get",
		"interface",
		ifc,
		"ofport",
	)
}

func parseOfport(ofport string) (uint16, error) {
	ofp, err := strconv.ParseUint(ofport, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(ofp), nil
}

func newAddFlowToBridgeCmd(bridge, flow string) *exec.Cmd {
	return exec.Command("ovs-ofctl", "add-flow", bridge, flow)
}

func (f *ovsFabric) Name() string {
	return ovs
}

func (f *ovsFabric) CreateLocalIfc(ifc NetworkInterface) (err error) {
	if err = f.createIfc(ifc.Name, ifc.GuestMAC); err != nil {
		return
	}
	defer func() {
		// clean up executed in case retrieving the openflow port or adding the
		// flows fails. Needed to avoid leaking interfaces because there's no
		// guarantee that in case of error the client will retry to create ifc,
		// or, even if there's a retry, the ifc name might have changed
		if err != nil {
			// wait a little bit to reduce chances of another failure in case
			// OvS is experiencing transient failures
			time.Sleep(cleanupDelay)
			if cleanUpErr := f.deleteIfc(ifc.Name); cleanUpErr != nil {
				glog.Errorf("could not delete local interface %s during clean up after failure: %s",
					ifc.Name,
					cleanUpErr.Error())
			}
		}
	}()

	ofport, err := f.getIfcOfport(ifc.Name)
	if err != nil {
		return
	}

	if err = f.addLocalIfcFlows(ofport, ifc.VNI, ifc.GuestMAC, ifc.GuestIP); err != nil {
		return
	}

	return nil
}

func (f *ovsFabric) addLocalIfcFlows(ofport uint16, tunID uint32, dlDst net.HardwareAddr, arpTPA net.IP) error {
	tunnelingFlow := fmt.Sprintf("table=0,in_port=%d,actions=set_tunnel:%d,resubmit(,1)",
		ofport,
		tunID)
	dlTrafficFlow := fmt.Sprintf("table=1,tun_id=%d,dl_dst=%s,actions=output:%d",
		tunID,
		dlDst,
		ofport)
	arpFlow := fmt.Sprintf("table=1,tun_id=%d,arp,arp_tpa=%s,actions=output:%d",
		tunID,
		arpTPA,
		ofport)

	addFlows := f.newAddFlowsInTransactionCmd(tunnelingFlow, dlTrafficFlow, arpFlow)

	if out, err := addFlows.CombinedOutput(); err != nil {
		return newAddFlowsInTransactionErr(strings.TrimRight(strings.Join([]string{tunnelingFlow, dlTrafficFlow, arpFlow}, " "), " "),
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) createIfc(ifc string, mac net.HardwareAddr) error {
	createIfc := newCreateBridgeIfcCmd(f.bridge, ifc, mac)

	if out, err := createIfc.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create local ifc %s with MAC %s and plug it into bridge %s: %s: %s",
			ifc,
			mac,
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) deleteIfc(ifc string) error {
	// the interface is managed by OvS (interface type internal), hence deleting
	// the bridge port it is associated with automatically deletes it
	deleteIfc := newDeleteBridgePortCmd(f.bridge, ifc)

	if out, err := deleteIfc.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete local ifc %s attached to bridge %s: %s: %s",
			name,
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func newCreateBridgeIfcCmd(bridge, ifc string, mac net.HardwareAddr) *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"--may-exist",
		"add-port",
		bridge,
		ifc,
		"--",
		"set",
		"interface",
		"ifc",
		"type=internal",
		"mac=02:00:00:00:00:13")
}

func newDeleteBridgePortCmd(bridge, ifc string) *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"del-port",
		bridge,
		ifc)
}

func (f *ovsFabric) newAddFlowsInTransactionCmd(flows ...string) *exec.Cmd {
	return exec.Command("ovs-ofctl",
		"--bundle",
		"add-flows",
		f.bridge,
		"<<EOF"+newLine+strings.Join(flows, newLine)+"EOF")
}

func (f *ovsFabric) DeleteLocalIfc(ifc NetworkInterface) error {
	ofport, err := f.getIfcOfport(ifc.Name)
	if err != nil {
		return err
	}

	if err := f.deleteLocalIfcFlows(ofport, ifc.VNI, ifc.GuestMAC, ifc.GuestIP); err != nil {
		return err
	}

	if err := f.deleteIfc(ifc.Name); err != nil {
		return err
	}

	return nil
}

func (f *ovsFabric) deleteLocalIfcFlows(ofport uint16, tunID uint32, dlDst net.HardwareAddr, arpTPA net.IP) error {
	tunnelingFlow := fmt.Sprintf("in_port=%d", ofport)
	dlTrafficFlow := fmt.Sprintf("tun_id=%d,dl_dst=%s", tunID, dlDst)
	arpFlow := fmt.Sprintf("tun_id=%d,arp,arp_tpa=%s", tunID, arpTPA)

	delFlows := f.newDelFlowsInTransactionCmd(tunnelingFlow, dlTrafficFlow, arpFlow)

	if out, err := delFlows.CombinedOutput(); err != nil {
		return newDelFlowsErr(strings.TrimRight(strings.Join([]string{tunnelingFlow, dlTrafficFlow, arpFlow}, " "), " "),
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) newDelFlowsInTransactionCmd(flows ...string) *exec.Cmd {
	return exec.Command("ovs-ofctl",
		"--bundle",
		"del-flows",
		f.bridge,
		"<<EOF"+newLine+strings.Join(flows, newLine)+"EOF")
}

func (f *ovsFabric) CreateRemoteIfc(ifc NetworkInterface) error {
	if err := f.addRemoteARPResolutionFlow(ifc.VNI, ifc.GuestIP, ifc.HostIP); err != nil {
		return err
	}

	return f.addRemoteDlTrafficFlow(ifc.VNI, ifc.GuestMAC, ifc.HostIP)
}

func (f *ovsFabric) DeleteRemoteIfc(ifc NetworkInterface) error {
	if err := f.delRemoteARPResolutionFlow(ifc.VNI, ifc.GuestIP); err != nil {
		return err
	}

	return f.delRemoteDlTrafficFlow(ifc.VNI, ifc.GuestMAC)
}

func (f *ovsFabric) ListLocalIfcs() ([]NetworkInterface, error) {
	// TODO implement
	return nil, nil
}

func (f *ovsFabric) ListRemoteIfcs() ([]NetworkInterface, error) {
	// TODO implement
	return nil, nil
}

func (f *ovsFabric) plugIfcInBridge(ifc string) error {
	plugIfcInBridgeCmd := newPlugIfcInBridgeCmd(f.bridge, ifc)

	if out, err := plugIfcInBridgeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to plug ifc %s into bridge %s: %s: %s",
			ifc,
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) addRemoteARPResolutionFlow(tunID uint32, arpTPA, tunDst net.IP) error {
	remoteARPResolution := fmt.Sprintf("table=1,tun_id=%d,arp,arp_tpa=%s,actions=set_field:%s->tun_dst,output:%d",
		tunID,
		arpTPA,
		tunDst,
		f.vtepOfport)

	addRemoteARPResolutionFlowCmd := newAddFlowToBridgeCmd(f.bridge, remoteARPResolution)
	if out, err := addRemoteARPResolutionFlowCmd.CombinedOutput(); err != nil {
		return newAddFlowToBridgeErr(remoteARPResolution,
			f.bridge,
			err.Error(),
			strings.TrimRight(string(out), newLine))
	}

	return nil
}

func (f *ovsFabric) addRemoteDlTrafficFlow(tunID uint32, dlDst net.HardwareAddr, tunDst net.IP) error {
	remoteDlTraffic := fmt.Sprintf("table=1,tun_id=%d,dl_dst=%s,actions=set_field:%s->tun_dst,output:%d",
		tunID,
		dlDst,
		tunDst,
		f.vtepOfport)

	addRemoteDlTrafficFlowCmd := newAddFlowToBridgeCmd(f.bridge, remoteDlTraffic)
	if out, err := addRemoteDlTrafficFlowCmd.CombinedOutput(); err != nil {
		return newAddFlowToBridgeErr(remoteDlTraffic,
			f.bridge,
			err.Error(),
			strings.TrimRight(string(out), newLine))
	}

	return nil
}

func (f *ovsFabric) delRemoteARPResolutionFlow(tunID uint32, arpTPA net.IP) error {
	remoteARPResolution := fmt.Sprintf("table=1,tun_id=%d,arp,arp_tpa=%s",
		tunID,
		arpTPA)

	delRemoteARPResolutionFlowCmd := newDelFlowFromBridgeCmd(f.bridge, remoteARPResolution)
	if out, err := delRemoteARPResolutionFlowCmd.CombinedOutput(); err != nil {
		return newDelFlowsErr(remoteARPResolution,
			f.bridge,
			err.Error(),
			strings.TrimRight(string(out), newLine))
	}

	return nil
}

func (f *ovsFabric) delRemoteDlTrafficFlow(tunID uint32, dlDst net.HardwareAddr) error {
	dlTraffic := fmt.Sprintf("table=1,tun_id=%d,dl_dst=%s",
		tunID,
		dlDst)

	delDlTrafficFlowCmd := newDelFlowFromBridgeCmd(f.bridge, dlTraffic)
	if out, err := delDlTrafficFlowCmd.CombinedOutput(); err != nil {
		return newDelFlowsErr(dlTraffic,
			f.bridge,
			err.Error(),
			strings.TrimRight(string(out), newLine))
	}

	return nil
}

func newDelFlowFromBridgeCmd(bridge, flow string) *exec.Cmd {
	return exec.Command("ovs-ofctl", "del-flows", bridge, flow)
}

func newPlugIfcInBridgeCmd(bridge, ifc string) *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"--may-exist",
		"add-port",
		bridge,
		ifc,
	)
}

type addFlowToBridgeErr struct {
	flow, bridge, msg string
}

func (e *addFlowToBridgeErr) Error() string {
	return fmt.Sprintf("failed to add OpenFlow flow %s to bridge \"%s\": %s",
		e.flow,
		e.bridge,
		e.msg)
}

func newAddFlowToBridgeErr(flow, bridge string, msgs ...string) *addFlowToBridgeErr {
	return &addFlowToBridgeErr{
		flow:   flow,
		bridge: bridge,
		msg:    strings.TrimRight(strings.Join(msgs, " "), " "),
	}
}

type addFlowInTransactionErr struct {
	flows, bridge, msg string
}

func (e *addFlowInTransactionErr) Error() string {
	return fmt.Sprintf("transaction to add OpenFlow flows %s to bridge \"%s\" failed: %s",
		e.flows,
		e.bridge,
		e.msg)
}

func newAddFlowsInTransactionErr(flows, bridge string, msgs ...string) *addFlowInTransactionErr {
	return &addFlowInTransactionErr{
		flows:  flows,
		bridge: bridge,
		msg:    strings.TrimRight(strings.Join(msgs, " "), " "),
	}
}

type delFlowsErr struct {
	flows, bridge, msg string
}

func (e *delFlowsErr) Error() string {
	return fmt.Sprintf("failed to delete OpenFlow flows %s to bridge \"%s\": %s",
		e.flows,
		e.bridge,
		e.msg)
}

func newDelFlowsErr(flows, bridge string, msgs ...string) *delFlowsErr {
	return &delFlowsErr{
		flows:  flows,
		bridge: bridge,
		msg:    strings.TrimRight(strings.Join(msgs, " "), " "),
	}
}
