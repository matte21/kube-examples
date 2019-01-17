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

// TODO During creation/config ifcs, add check to make sure config is similar to what we want (need to think whether we actually need this, I think not)

import (
	"bytes"
	"fmt"
	"github.com/golang/glog"
	"net"
	"os/exec"
	"regexp"
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
	// get map from local ifc name to its openflow port nbr
	nameToOfport, err := f.getLocalIfcsNamesToOfports()
	if err != nil {
		return nil, err
	}

	// get the flows associated with local ifcs
	flows, err := f.getFlows()
	if err != nil {
		return nil, err
	}
	localFlows := f.filterOutNonLocalFlows(flows)

	// use flows to map ofport to local ifc data(MAC, guestIP, VNI)
	ofportToIfcData, err := f.getOfportToLocalIfcData(localFlows)
	if err != nil {
		// TODO think whether it is better to simply log move on
		return nil, err
	}

	// for each name in nameToOfport, use its ofport to retrieve its ifc data
	// in ofportToIfcData. For each match, build the local ifc and add it to
	// the list of ifcs to return. If for a name no match is found, it means
	// that the connection agent crashed after creating the network device but
	// before adding the flows: it is not possible to build a complete NetworkInterface
	// struct (i.e. VNI and GuestIP are missing), hence we add the interface to
	// a list of "orphan" ifcs for subsequent deletion
	ifcsFound := make([]NetworkInterface, 0, len(ofportToIfcData))
	orphanIfcs := make([]string, 0, 0)
	for ifcName, ifcOfport := range nameToOfport {
		if ifcData, found := ofportToIfcData[ifcOfport]; found {
			ifc := NetworkInterface{
				Name:     ifcName,
				VNI:      ifcData.vni,
				GuestIP:  ifcData.guestIP,
				GuestMAC: ifcData.guestMAC,
			}
			ifcsFound = append(ifcsFound, ifc)
		} else {
			orphanIfcs = append(orphanIfcs, ifcName)
		}
	}

	// best effort attempt to delete orphan ifcs
	for _, anOrphanIfc := range orphanIfcs {
		if err := f.deleteIfc(anOrphanIfc); err == nil {
			glog.V(3).Infof("Deleted local interface %s (no flows associated with it could be found)",
				anOrphanIfc)
		} else {
			glog.Errorf("Failed to deleted local interface %s (no flows associated with it could be found): %s",
				anOrphanIfc,
				err.Error())
		}
	}
	return ifcsFound, nil
}

func (f *ovsFabric) getLocalIfcsNamesToOfports() (map[string]uint16, error) {
	listIfcNamesAndOfport := f.newListIfcNamesAndOfPortCmd()

	out, err := listIfcNamesAndOfport.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list local ifcs names and ofports: %s: %s",
			err.Error(),
			string(out))
	}

	// TODO factor out following code in a different, "parse" method
	ifcNamesAndOfports := bytes.Split(out, []byte("\n"))
	ifcNamesToOfports := make(map[string]uint16, len(ifcNamesAndOfports))
	for _, nameAndOfportBytes := range ifcNamesAndOfports {
		nameAndOfportJoined := string(nameAndOfportBytes)
		nameAndOfport := strings.Fields(nameAndOfportJoined)

		// TODO this check is not enough. If there's more than one OvS bridge the
		// interfaces of all bridges are retruned. Fix this.
		if nameAndOfport[0] != vtep && nameAndOfport[0] != f.bridge {
			ofport64, err := strconv.ParseUint(nameAndOfport[1], 10, 16)
			if err != nil {
				glog.Errorf("failed to parse openflow port nbr for local ifc %s",
					nameAndOfport[0])
			}
			ifcNamesToOfports[nameAndOfport[0]] = uint16(ofport64)
		}
	}

	return ifcNamesToOfports, nil
}

func (f *ovsFabric) getFlows() ([]string, error) {
	getFlows := f.newGetFlowsCmd()

	out, err := getFlows.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list flows for bridge %s: %s: %s",
			f.bridge,
			err.Error(),
			string(out))
	}

	// if we have a lot of flows we could be occupying a lot of memory
	flowsBytes := bytes.Split(out, []byte("\n"))
	flows := make([]string, 0, len(flowsBytes))
	for _, aFlow := range flowsBytes {
		flows = append(flows, string(aFlow))
	}

	return flows, nil
}

func (f *ovsFabric) filterOutNonLocalFlows(allFlows []string) (localFlows []string) {
	localFlows = make([]string, 0, 0)
	for _, aFlow := range allFlows {
		if f.isLocal(aFlow) {
			localFlows = append(localFlows, aFlow)
		}
	}
	return
}

func (f *ovsFabric) isLocal(flow string) bool {
	return strings.Contains(flow, "in_port") ||
		strings.Contains(flow, "actions=output:")
}

func (f *ovsFabric) newListIfcNamesAndOfPortCmd() *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"-f",
		"table",
		"--no-heading",
		"--",
		"--columns=name,ofport",
		"list",
		"Interface")
}

func (f *ovsFabric) newGetFlowsCmd() *exec.Cmd {
	// TODO maybe we can do better: some options might filter out the flows we
	// do not need
	return exec.Command("ovs-vsctl", "dump-flows", "kos")
}

func (f *ovsFabric) getOfportToLocalIfcData(flows []string) (map[uint16]localIfcFlowsData, error) {
	ofportToIfcData := make(map[uint16]localIfcFlowsData, len(flows)/3)
	var err error
	for _, aFlow := range flows {
		// TODO use pointers for ifcData, time and memory cost will decrease
		// by a fair amount if there are a lot of flows
		switch {
		case strings.Contains(aFlow, "in_port"):
			ofport, vni, err := f.parseTunnelingFlow(aFlow)
			if err == nil {
				ifcData, _ := ofportToIfcData[ofport]
				ifcData.vni = vni
				ofportToIfcData[ofport] = ifcData
			}
		case strings.Contains(aFlow, "dl_dst"):
			ofport, guestMAC, err := f.parseLocalDlTrafficFlow(aFlow)
			if err == nil {
				ifcData, _ := ofportToIfcData[ofport]
				ifcData.guestMAC = guestMAC
				ofportToIfcData[ofport] = ifcData
			}
		case strings.Contains(aFlow, "arp"):
			ofport, guestIP, err := f.parseLocalARPFlow(aFlow)
			if err == nil {
				ifcData, _ := ofportToIfcData[ofport]
				ifcData.guestIP = guestIP
				ofportToIfcData[ofport] = ifcData
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return ofportToIfcData, nil
}

func (f *ovsFabric) parseTunnelingFlow(flow string) (ofport uint16, tunID uint32, err error) {
	// extract ofport
	inPortRegexp, err := regexp.Compile("in_port=[0-9]+")
	if err != nil {
		return
	}
	nbrRegexp, err := regexp.Compile("[0-9]+")
	if err != nil {
		return
	}
	ofportStr := nbrRegexp.FindString(inPortRegexp.FindString(flow))
	ofport64, err := strconv.ParseUint(ofportStr, 10, 16)
	if err != nil {
		return
	}
	ofport = uint16(ofport64)

	// extract tunnel ID
	setFieldRegexp, err := regexp.Compile("set_field:[0-9]+")
	if err != nil {
		return
	}
	tunIDStr := nbrRegexp.FindString(setFieldRegexp.FindString(flow))
	tunID64, err := strconv.ParseUint(tunIDStr, 10, 32)
	if err != nil {
		return
	}
	tunID = uint32(tunID64)

	return
}

func (f *ovsFabric) parseLocalDlTrafficFlow(flow string) (ofport uint16, dlDst net.HardwareAddr, err error) {
	// TODO factor out extraction of ofport in a different method and use that
	// extract ofport
	inPortRegexp, err := regexp.Compile("output:[0-9]+")
	if err != nil {
		return
	}
	ofportRegexp, err := regexp.Compile("[0-9]+")
	if err != nil {
		return
	}
	ofportStr := ofportRegexp.FindString(inPortRegexp.FindString(flow))
	ofport64, err := strconv.ParseUint(ofportStr, 10, 16)
	if err != nil {
		return
	}
	ofport = uint16(ofport64)

	// extract dlDst
	dlDstKeyValPairRegexp, err := regexp.Compile("dl_dst=*,")
	if err != nil {
		return
	}
	dlDstValRegexp, err := regexp.Compile("^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$")
	if err != nil {
		return
	}
	dlDstStr := dlDstValRegexp.FindString(dlDstKeyValPairRegexp.FindString(flow))
	dlDst, err = net.ParseMAC(dlDstStr)
	return
}

func (f *ovsFabric) parseLocalARPFlow(flow string) (ofport uint16, arpTPA net.IP, err error) {
	// TODO factor out extraction of ofport in a different method and use that
	// extract ofport
	inPortRegexp, err := regexp.Compile("output:[0-9]+")
	if err != nil {
		return
	}
	ofportRegexp, err := regexp.Compile("[0-9]+")
	if err != nil {
		return
	}
	ofportStr := ofportRegexp.FindString(inPortRegexp.FindString(flow))
	ofport64, err := strconv.ParseUint(ofportStr, 10, 16)
	if err != nil {
		return
	}
	ofport = uint16(ofport64)

	// extract dlDst
	arpTPAKeyValPairRegexp, err := regexp.Compile("arp_tpa=*,")
	if err != nil {
		return
	}
	arpTPAValRegexp, err := regexp.Compile("^(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$")
	if err != nil {
		return
	}
	arpTPAStr := arpTPAValRegexp.FindString(arpTPAKeyValPairRegexp.FindString(flow))
	arpTPA = net.ParseIP(arpTPAStr)
	if arpTPA == nil {
		err = fmt.Errorf("%s is not a valid IPv4 address", arpTPA)
	}
	return
}

// localIfcFlowsData represents the data about a local interface that can
// be inferred from the interface flows
type localIfcFlowsData struct {
	vni      uint32
	guestIP  net.IP
	guestMAC net.HardwareAddr
}

func (f *ovsFabric) ListRemoteIfcs() ([]NetworkInterface, error) {
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
