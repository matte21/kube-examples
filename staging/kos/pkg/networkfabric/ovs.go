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

// TODO refactor creation of commands and flows - use templates if it helps
// TODO During creation/config ifcs, add check to make sure config is similar to what we want (need to think whether we actually need this, I think not)

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
	createBridge := f.newCreateBridgeCmd()

	if out, err := createBridge.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create bridge %s: %s: %s",
			f.bridge,
			err.Error(),
			string(out))
	}
	glog.V(4).Infof("Created OvS bridge %s", f.bridge)

	return nil
}

func (f *ovsFabric) addVTEP() error {
	addVTEP := f.newAddVTEPCmd()

	if out, err := addVTEP.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add VTEP port and interface to bridge %s: %s: %s",
			f.bridge,
			err.Error(),
			string(out))
	}
	glog.V(4).Infof("Added VTEP to OvS bridge %s", f.bridge)

	return nil
}

func (f *ovsFabric) addDefaultFlows() error {
	defaultResubmitToT1Flow := "table=0,actions=resubmit(,1)"
	defaultDropFlow := "table=1,actions=drop"

	addFlows := f.newAddFlowsCmd(defaultResubmitToT1Flow, defaultDropFlow)

	if out, err := addFlows.CombinedOutput(); err != nil {
		return newAddFlowsErr(strings.Join([]string{defaultResubmitToT1Flow, defaultDropFlow}, " "),
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) newCreateBridgeCmd() *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"--may-exist",
		"add-br",
		f.bridge,
		"--",
		"set",
		"bridge",
		f.bridge,
		"protocols=OpenFlow10,OpenFlow11,OpenFlow12,OpenFlow13,OpenFlow14,OpenFlow15")
}

func (f *ovsFabric) newAddVTEPCmd() *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"--may-exist",
		"add-port",
		f.bridge,
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
	getIfcOfport := f.newGetIfcOfportCmd(ifc)

	outBytes, err := getIfcOfport.CombinedOutput()
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

func (f *ovsFabric) newGetIfcOfportCmd(ifc string) *exec.Cmd {
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
	tunnelingFlow := fmt.Sprintf("table=0,in_port=%d,actions=set_field:%d->tun_id,resubmit(,1)",
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

	addFlows := f.newAddFlowsCmd(tunnelingFlow, dlTrafficFlow, arpFlow)

	if out, err := addFlows.CombinedOutput(); err != nil {
		return newAddFlowsErr(strings.Join([]string{tunnelingFlow, dlTrafficFlow, arpFlow}, " "),
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) createIfc(ifc string, mac net.HardwareAddr) error {
	createIfc := f.newCreateBridgeIfcCmd(ifc, mac)

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
	deleteIfc := f.newDeleteBridgePortCmd(ifc)

	if out, err := deleteIfc.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete local ifc %s attached to bridge %s: %s: %s",
			name,
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) newCreateBridgeIfcCmd(ifc string, mac net.HardwareAddr) *exec.Cmd {
	// TODO think thoroughly if we need more than just the may exist flag
	// (e.g. detecting the case and reacting appropriately)
	return exec.Command("ovs-vsctl",
		"--may-exist",
		"add-port",
		f.bridge,
		ifc,
		"--",
		"set",
		"interface",
		ifc,
		"type=internal",
		fmt.Sprintf("mac=%s", strings.Replace(mac.String(), ":", "\\:", -1)))
}

func (f *ovsFabric) newDeleteBridgePortCmd(ifc string) *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"del-port",
		f.bridge,
		ifc)
}

func (f *ovsFabric) newAddFlowsCmd(flows ...string) *exec.Cmd {
	// the --bundle flag makes the addition of the flows transactional
	cmd := exec.Command("ovs-ofctl", "--bundle", "add-flows", f.bridge, "-")
	cmd.Stdin = strings.NewReader(strings.Join(flows, newLine) + newLine)
	return cmd
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
		// should we re-insert the flows? I say no
		return err
	}

	return nil
}

func (f *ovsFabric) deleteLocalIfcFlows(ofport uint16, tunID uint32, dlDst net.HardwareAddr, arpTPA net.IP) error {
	tunnelingFlow := fmt.Sprintf("in_port=%d", ofport)
	dlTrafficFlow := fmt.Sprintf("tun_id=%d,dl_dst=%s", tunID, dlDst)
	arpFlow := fmt.Sprintf("tun_id=%d,arp,arp_tpa=%s", tunID, arpTPA)

	delFlows := f.newDelFlowsCmd(tunnelingFlow, dlTrafficFlow, arpFlow)

	if out, err := delFlows.CombinedOutput(); err != nil {
		return newDelFlowsErr(strings.TrimRight(strings.Join([]string{tunnelingFlow, dlTrafficFlow, arpFlow}, " "), " "),
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) newDelFlowsCmd(flows ...string) *exec.Cmd {
	// the --bundle flag makes the deletion of the flows transactional
	cmd := exec.Command("ovs-ofctl", "--bundle", "del-flows", f.bridge, "-")
	cmd.Stdin = strings.NewReader(strings.Join(flows, newLine) + newLine)
	return cmd
}

func (f *ovsFabric) CreateRemoteIfc(ifc NetworkInterface) error {
	return f.addRemoteIfcFlows(ifc.VNI, ifc.GuestMAC, ifc.HostIP, ifc.GuestIP)
}

func (f *ovsFabric) DeleteRemoteIfc(ifc NetworkInterface) error {
	return f.deleteRemoteIfcFlows(ifc.VNI, ifc.GuestMAC, ifc.GuestIP)
}

func (f *ovsFabric) addRemoteIfcFlows(tunID uint32, dlDst net.HardwareAddr, tunDst, arpTPA net.IP) error {
	dlTrafficFlow := fmt.Sprintf("table=1,tun_id=%d,dl_dst=%s,actions=set_field:%s->tun_dst,output:%d",
		tunID,
		dlDst,
		tunDst,
		f.vtepOfport)
	arpFlow := fmt.Sprintf("table=1,tun_id=%d,arp,arp_tpa=%s,actions=set_field:%s->tun_dst,output:%d",
		tunID,
		arpTPA,
		tunDst,
		f.vtepOfport)

	addFlows := f.newAddFlowsCmd(dlTrafficFlow, arpFlow)

	if out, err := addFlows.CombinedOutput(); err != nil {
		return newAddFlowsErr(strings.Join([]string{dlTrafficFlow, arpFlow}, " "),
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) deleteRemoteIfcFlows(tunID uint32, dlDst net.HardwareAddr, arpTPA net.IP) error {
	dlTrafficFlow := fmt.Sprintf("table=1,tun_id=%d,dl_dst=%s",
		tunID,
		dlDst)
	arpFlow := fmt.Sprintf("table=1,tun_id=%d,arp,arp_tpa=%s",
		tunID,
		arpTPA)

	delFlows := f.newDelFlowsCmd(dlTrafficFlow, arpFlow)

	if out, err := delFlows.CombinedOutput(); err != nil {
		return newDelFlowsErr(strings.Join([]string{dlTrafficFlow, arpFlow}, " "),
			f.bridge,
			err.Error(),
			string(out))
	}

	return nil
}

func (f *ovsFabric) ListLocalIfcs() ([]NetworkInterface, error) {
	// list interface names and ofport
	// list flows
	// for each ifc: retrieve the 2 flows whose actions is only output:ofport and match them
	// delete interfaces left
	// TODO undeploy/delete bridge
	return nil, nil
}

func (f *ovsFabric) ListRemoteIfcs() ([]NetworkInterface, error) {
	// TODO implement
	return nil, nil
}

type addFlowsErr struct {
	flows, bridge, msg string
}

func (e *addFlowsErr) Error() string {
	return fmt.Sprintf("transaction to add OpenFlow flows %s to bridge %s failed: %s",
		e.flows,
		e.bridge,
		e.msg)
}

func newAddFlowsErr(flows, bridge string, msgs ...string) *addFlowsErr {
	return &addFlowsErr{
		flows:  flows,
		bridge: bridge,
		msg:    strings.Join(msgs, " "),
	}
}

type delFlowsErr struct {
	flows, bridge, msg string
}

func (e *delFlowsErr) Error() string {
	return fmt.Sprintf("transaction to delete OpenFlow flows %s to bridge %s failed: %s",
		e.flows,
		e.bridge,
		e.msg)
}

func newDelFlowsErr(flows, bridge string, msgs ...string) *delFlowsErr {
	return &delFlowsErr{
		flows:  flows,
		bridge: bridge,
		msg:    strings.Join(msgs, " "),
	}
}
