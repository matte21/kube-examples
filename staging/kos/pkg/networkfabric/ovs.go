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

import (
	"encoding/binary"
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
	ovs = "ovs"

	// time to wait after a failure before performing the needed clean up
	cleanupDelay = 1 * time.Second

	// string templates for regexps used to parse the OpenFlow flows
	decNbrRegexpStr = "[0-9]+"
	inPortRegexpStr = "in_port=" + decNbrRegexpStr
	outputRegexpStr = "output:" + decNbrRegexpStr
	hexNbrRegexpStr = "0[xX][0-9a-fA-F]+"
	tunIDRegexpStr  = "tun_id=" + hexNbrRegexpStr
	loadRegexpStr   = "load:" + hexNbrRegexpStr
	cookieRegexpStr = "cookie=" + hexNbrRegexpStr
	ipv4RegexpStr   = "(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])"
	arpTPARegexpStr = "arp_tpa=" + ipv4RegexpStr
	macRegexpStr    = "([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})"

	// the command to dump the OpenFlow flows contains some hex numbers prefixed
	// by 0x. Store 0x in a const used to remove such leading chars before
	// parsing hex numbers
	hexPrefixChars = "0xX"

	// remoteFlowsFingerprint stores a string (the name of the tunnel
	// destination field) that all and only the flows created for remote
	// interfaces contain: use it to identify such flows
	remoteFlowsFingerprint = "NXM_NX_TUN_IPV4_DST"
	arpFlowsFingerprint    = "arp"
)

type ovsFabric struct {
	bridge         string
	vtep           string
	vtepOfport     uint16
	flowParsingKit *regexpKit
}

type regexpKit struct {
	decNbr *regexp.Regexp
	inPort *regexp.Regexp
	output *regexp.Regexp
	hexNbr *regexp.Regexp
	tunID  *regexp.Regexp
	load   *regexp.Regexp
	cookie *regexp.Regexp
	ipv4   *regexp.Regexp
	arpTPA *regexp.Regexp
	mac    *regexp.Regexp
}

func init() {
	if f, err := NewOvSFabric("kos"); err != nil {
		panic(fmt.Sprintf("failed to create OvS network fabric: %s", err.Error()))
	} else {
		registerFabric(ovs, f)
	}
}

// NewOvSFabric returns a network fabric for creating local and remote interfaces
// based on Openvswitch. It creates its own OvS bridge with name bridge.
func NewOvSFabric(bridge string) (*ovsFabric, error) {
	f := &ovsFabric{}
	f.initFlowsParsingKit()
	glog.V(6).Infof("Initialized bridge %s flows parsing kit", bridge)
	if err := f.initBridge(bridge); err != nil {
		return nil, err
	}
	glog.V(2).Infof("Initialized bridge %s", bridge)
	return f, nil
}

func (f *ovsFabric) Name() string {
	return ovs
}

func (f *ovsFabric) CreateLocalIfc(ifc LocalNetIfc) (err error) {
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
				glog.Errorf("Could not delete local interface %s during clean up after failure: %s",
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

	glog.V(2).Infof("Created local interface %#+v connected to bridge %s",
		ifc,
		f.bridge)

	return nil
}

func (f *ovsFabric) DeleteLocalIfc(ifc LocalNetIfc) error {
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

	glog.V(2).Infof("Deleted local interface %#+v connected to bridge %s",
		ifc,
		f.bridge)

	return nil
}

func (f *ovsFabric) CreateRemoteIfc(ifc RemoteNetIfc) error {
	return f.addRemoteIfcFlows(ifc.VNI, ifc.GuestMAC, ifc.HostIP, ifc.GuestIP)
}

func (f *ovsFabric) DeleteRemoteIfc(ifc RemoteNetIfc) error {
	return f.deleteRemoteIfcFlows(ifc.VNI, ifc.GuestMAC, ifc.GuestIP)
}

func (f *ovsFabric) ListLocalIfcs() ([]LocalNetIfc, error) {
	// build a map from openflow port nbr to local ifc name
	ofportToIfcName, err := f.getOfportsToLocalIfcNames()
	if err != nil {
		return nil, err
	}

	// useful flows associated with local interfaces are those for ARP and normal
	// datalink traffic. The one for tunneling is useless, it only carries the
	// VNI of an interface, which is stored in the two other flows as well
	localFlows, err := f.getUsefulLocalFlows()
	if err != nil {
		return nil, err
	}

	// build a map from the openflow port of an interface to the two useful
	// flows it is associated with
	glog.V(4).Infof("Pairing ARP and normal Datalink traffic flows of local interfaces in bridge %s...",
		f.bridge)
	ofportToLocalFlowsPairs := f.ofportToLocalFlowsPairs(localFlows)

	// use the map from ofports to pairs of flows to build a map from ofport
	// to LocalNetIfc structs with all the fields set but the name (because no
	// flow carries info about the interface name)
	glog.V(4).Infof("Parsing flows pairs found in bridge %s into local Network Interfaces...",
		f.bridge)
	ofportToNamelessIfc := f.parseLocalFlowsPairs(ofportToLocalFlowsPairs)

	// assign a name to the nameless ifcs built with the previous instruction.
	// completeIfcs are those to return, orphanIfcs are interfaces for whom
	// flows could not be found. Such interfaces are created if the connection
	// agent crashes between the creation of the interface and the addition of
	// its flows, or if the latter fails for whatever reason and also deleting
	// the interface for clean up fails.
	glog.V(4).Infof("Naming local Network Interfaces parsed out of bridge %s...",
		f.bridge)
	completeIfcs, orphanIfcs := f.nameIfcs(ofportToIfcName, ofportToNamelessIfc)

	// best effort attempt to delete orphan interfaces
	glog.V(4).Infof("Deleting network devices connected to bridge %s for whom OpenFlow flows were not be found...",
		f.bridge)
	f.deleteOrphanIfcs(orphanIfcs)

	return completeIfcs, nil
}

func (f *ovsFabric) ListRemoteIfcs() ([]RemoteNetIfc, error) {
	remoteFlows, err := f.getRemoteFlows()
	if err != nil {
		return nil, err
	}

	// each remote interface is associated with two flows. Arrange flows in pairs
	// by interface.
	glog.V(4).Infof("Pairing ARP and normal Datalink traffic flows of remote interfaces in bridge %s...",
		f.bridge)
	perIfcFlowPairs := f.pairRemoteFlowsPerIfc(remoteFlows)

	return f.parseRemoteFlowsPairs(perIfcFlowPairs), nil
}

func (f *ovsFabric) initFlowsParsingKit() {
	f.flowParsingKit = &regexpKit{
		decNbr: regexp.MustCompile(decNbrRegexpStr),
		inPort: regexp.MustCompile(inPortRegexpStr),
		output: regexp.MustCompile(outputRegexpStr),
		hexNbr: regexp.MustCompile(hexNbrRegexpStr),
		tunID:  regexp.MustCompile(tunIDRegexpStr),
		load:   regexp.MustCompile(loadRegexpStr),
		cookie: regexp.MustCompile(cookieRegexpStr),
		ipv4:   regexp.MustCompile(ipv4RegexpStr),
		arpTPA: regexp.MustCompile(arpTPARegexpStr),
		mac:    regexp.MustCompile(macRegexpStr),
	}
}

func (f *ovsFabric) initBridge(name string) error {
	f.bridge = name
	f.vtep = "vtep"

	if err := f.createBridge(); err != nil {
		return err
	}

	if err := f.addVTEP(); err != nil {
		return err
	}

	vtepOfport, err := f.getIfcOfport(f.vtep)
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
	glog.V(4).Infof("Added VTEP to bridge %s", f.bridge)

	return nil
}

func (f *ovsFabric) getIfcOfport(ifc string) (uint16, error) {
	getIfcOfport := f.newGetIfcOfportCmd(ifc)

	outBytes, err := getIfcOfport.CombinedOutput()
	out := strings.TrimRight(string(outBytes), "\n")
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve OpenFlow port of interface %s in bridge %s: %s: %s",
			ifc,
			f.bridge,
			err.Error(),
			out)
	}

	ofport, err := parseOfport(out)
	if err != nil {
		return 0, err
	}
	glog.V(4).Infof("Retrieved OpenFlow port nbr (%d) of network device %s in bridge %s",
		ofport,
		ifc,
		f.bridge)

	return ofport, nil
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

func (f *ovsFabric) createIfc(ifc string, mac net.HardwareAddr) error {
	createIfc := f.newCreateIfcCmd(ifc, mac)

	if out, err := createIfc.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create local ifc %s with MAC %s and plug it into bridge %s: %s: %s",
			ifc,
			mac,
			f.bridge,
			err.Error(),
			string(out))
	}

	glog.V(4).Infof("Created network device %s with MAC %s connected to bridge %s",
		ifc,
		mac,
		f.bridge)

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

	glog.V(4).Infof("Deleted network device %s connected to bridge %s",
		ifc,
		f.bridge)

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

	glog.V(4).Infof("Bridge %s: added OpenFlow flows: \n\t%s\n\t%s\n\t%s",
		f.bridge,
		tunnelingFlow,
		dlTrafficFlow,
		arpFlow)

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

	glog.V(4).Infof("Bridge %s: deleted OpenFlow flows: \n\t%s\n\t%s\n\t%s",
		f.bridge,
		tunnelingFlow,
		dlTrafficFlow,
		arpFlow)

	return nil
}

func (f *ovsFabric) addRemoteIfcFlows(tunID uint32, dlDst net.HardwareAddr, tunDst, arpTPA net.IP) error {
	// cookies are an opaque numeric ID that OpenFlow offers to group together
	// flows. Here we use one to link together the two flows created for the
	// same remote interface. It is computed as a function of dlDst (the MAC of
	// the interface), making it impossible for two flows created out of different
	// remote interfaces to have the same one. It is needed because the essential
	// fields in the two flows created do not overlap. Without it it's
	// impossible to pair remote flows that were originated by the same interface,
	// but we need this coupling at remote interfaces list time.
	cookie, _ := binary.Uvarint(dlDst)

	dlTrafficFlow := fmt.Sprintf("table=1,cookie=%d,tun_id=%d,dl_dst=%s,actions=set_field:%s->tun_dst,output:%d",
		cookie,
		tunID,
		dlDst,
		tunDst,
		f.vtepOfport)
	arpFlow := fmt.Sprintf("table=1,cookie=%d,tun_id=%d,arp,arp_tpa=%s,actions=set_field:%s->tun_dst,output:%d",
		cookie,
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

	glog.V(4).Infof("Bridge %s: added OpenFlow flows: \n\t%s\n\t%s",
		f.bridge,
		dlTrafficFlow,
		arpFlow)

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

	glog.V(4).Infof("Bridge %s: deleted OpenFlow flows: \n\t%s\n\t%s",
		f.bridge,
		dlTrafficFlow,
		arpFlow)

	return nil
}

func parseOfport(ofport string) (uint16, error) {
	ofp, err := strconv.ParseUint(ofport, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(ofp), nil
}

func (f *ovsFabric) getOfportsToLocalIfcNames() (map[uint16]string, error) {
	listOfportsAndIfcNames := f.newListOfportsAndIfcNamesCmd()

	out, err := listOfportsAndIfcNames.CombinedOutput()
	outStr := strings.TrimRight(string(out), "\n")
	if err != nil {
		return nil, fmt.Errorf("failed to list local ifcs names and ofports: %s: %s",
			err.Error(),
			outStr)
	}

	glog.V(4).Infof("Parsing OpenFlow ports and Interface names in bridge %s...",
		f.bridge)

	return f.parseOfportsAndIfcNames(outStr), nil
}

func (f *ovsFabric) getUsefulLocalFlows() ([]string, error) {
	flows, err := f.getFlows()
	if err != nil {
		return nil, err
	}
	return onlyUsefulLocalFlowsFunc(flows), nil
}

func (f *ovsFabric) getRemoteFlows() ([]string, error) {
	flows, err := f.getFlows()
	if err != nil {
		return nil, err
	}
	return onlyRemoteFlowsFunc(flows), nil
}

func (f *ovsFabric) ofportToLocalFlowsPairs(flows []string) map[uint16][]string {
	ofportToFlowsPairs := make(map[uint16][]string, len(flows)/2)
	for _, aFlow := range flows {
		ofport := f.usefulLocalFlowOfport(aFlow)
		ofportToFlowsPairs[ofport] = append(ofportToFlowsPairs[ofport], aFlow)
		if len(ofportToFlowsPairs[ofport]) == 2 {
			glog.V(5).Infof("Paired flows \"%s\" \"%s\"", ofportToFlowsPairs[ofport][0], ofportToFlowsPairs[ofport][1])
		}
	}
	return ofportToFlowsPairs
}

func (f *ovsFabric) parseLocalFlowsPairs(ofportToPair map[uint16][]string) map[uint16]*LocalNetIfc {
	ofportToIfc := make(map[uint16]*LocalNetIfc, len(ofportToPair))
	for ofport, aPair := range ofportToPair {
		glog.V(5).Infof("Parsing flows pair \"%s\" \"%s\"...", aPair[0], aPair[1])
		ofportToIfc[ofport] = f.parseLocalFlowPair(aPair)
	}
	return ofportToIfc
}

func (f *ovsFabric) pairRemoteFlowsPerIfc(flows []string) [][]string {
	flowPairs := make(map[string][]string, len(flows)/2)
	for _, aFlow := range flows {
		// two flows belong to the same pair if they have the same cookie
		flowPair := flowPairs[f.extractCookie(aFlow)]
		flowPair = append(flowPair, aFlow)
		if len(flowPair) == 2 {
			glog.V(5).Infof("Paired flows \"%s\" \"%s\"", flowPair[0], flowPair[1])
		}
	}

	// we don't need a map where the key is the cookie. We only need flow pairs,
	// hence we store them in a slice of slices (each pair is stored in an
	// innermost slice)
	perIfcFlowPairs := make([][]string, len(flowPairs))
	for _, aFlowPair := range flowPairs {
		perIfcFlowPairs = append(perIfcFlowPairs, aFlowPair)
	}
	return perIfcFlowPairs
}

func (f *ovsFabric) parseRemoteFlowsPairs(flowsPairs [][]string) []RemoteNetIfc {
	ifcs := make([]RemoteNetIfc, len(flowsPairs))
	for _, aPair := range flowsPairs {
		glog.V(6).Infof("Parsing flows pair \"%s\" \"%s\"...", aPair[0], aPair[1])
		ifcs = append(ifcs, f.parseRemoteFlowPair(aPair))
	}
	return ifcs
}

func (f *ovsFabric) nameIfcs(ofportToIfcName map[uint16]string, ofportToIfc map[uint16]*LocalNetIfc) ([]LocalNetIfc, []string) {
	completeIfcs := make([]LocalNetIfc, len(ofportToIfc))
	orphanIfcs := make([]string, 0)
	for ofport, name := range ofportToIfcName {
		ifc := ofportToIfc[ofport]
		if ifc == nil {
			orphanIfcs = append(orphanIfcs, name)
			glog.V(5).Infof("No flows found for network device %s connected to bridge %s",
				name,
				f.bridge)
		} else {
			ifc.Name = name
			completeIfcs = append(completeIfcs, *ifc)
			glog.V(5).Infof("Named local network interface %#+v",
				ifc)
		}
	}
	return completeIfcs, orphanIfcs
}

func (f *ovsFabric) deleteOrphanIfcs(ifcs []string) {
	for _, anIfc := range ifcs {
		if err := f.deleteIfc(anIfc); err != nil {
			glog.Errorf("Failed to delete interface %s from bridge %s: %s. Deletion needed because no flows for the interface were found",
				anIfc,
				f.bridge,
				err.Error())
		} else {
			glog.V(5).Infof("Deleted interface %s from bridge %s: no flows found",
				anIfc,
				f.bridge)
		}
	}
}

func (f *ovsFabric) parseOfportsAndIfcNames(ofportsAndIfcNamesRaw string) map[uint16]string {
	ofportsAndIfcNames := strings.Split(ofportsAndIfcNamesRaw, "\n")
	ofportToIfcName := make(map[uint16]string, len(ofportsAndIfcNames))

	for _, anOfportAndIfcNamePair := range ofportsAndIfcNames {
		f.parseOfportAndIfcName(anOfportAndIfcNamePair, ofportToIfcName)
	}

	return ofportToIfcName
}

func (f *ovsFabric) parseOfportAndIfcName(ofportAndNameJoined string, ofportToIfcName map[uint16]string) {
	ofportAndName := strings.Fields(ofportAndNameJoined)

	glog.V(5).Infof("Parsing OpenFlow port number and interface pair %s from bridge %s:",
		ofportAndName,
		f.bridge)

	// TODO this check is not enough. If there's more than one OvS bridge the
	// interfaces of all bridges are returned, and we might add interfaces which
	// are not part of the bridge this fabric refers to. Fix this. Investigate
	// whether there's an OvS cli option to get interface names and ports from a
	// single bridge. If not think about something else (we could define the
	// interface name to be of a different type than just strings, that enforces
	// a certain pattern). Or we could have this fabric set the name rather than
	// the connection agent.
	if ofportAndName[1] != f.vtep && ofportAndName[1] != f.bridge {
		ofport64, _ := strconv.ParseUint(ofportAndName[0], 10, 16)
		ofportToIfcName[uint16(ofport64)] = ofportAndName[1]
	}
}

func (f *ovsFabric) getFlows() ([]string, error) {
	getFlows := f.newGetFlowsCmd()

	out, err := getFlows.CombinedOutput()
	outStr := strings.TrimRight(string(out), "\n")
	if err != nil {
		return nil, fmt.Errorf("failed to list flows for bridge %s: %s: %s",
			f.bridge,
			err.Error(),
			outStr)
	}

	return parseGetFlowsOutput(outStr), nil
}

// the trailing "Func" stands for functional: this function returns a new slice
// backed by a new array wrt the input slice. A useful flow is a flow which is
// not a tunneling flow, that is, ARP and normal datalink traffic flows
func onlyUsefulLocalFlowsFunc(flows []string) (localFlows []string) {
	localFlows = make([]string, 0, 0)
	for _, aFlow := range flows {
		if isLocal(aFlow) && !isTunneling(aFlow) {
			localFlows = append(localFlows, aFlow)
		}
	}
	return
}

// the trailing "Func" stands for functional: this function returns a new slice
// backed by a new array wrt the input slice
func onlyRemoteFlowsFunc(flows []string) (remoteFlows []string) {
	remoteFlows = make([]string, 0)
	for _, aFlow := range flows {
		if isRemote(aFlow) {
			remoteFlows = append(remoteFlows, aFlow)
		}
	}
	return
}

func isLocal(flow string) bool {
	return strings.Contains(flow, "in_port") ||
		strings.Contains(flow, "actions=output:")
}

func isRemote(flow string) bool {
	return strings.Contains(flow, remoteFlowsFingerprint)
}

func isARP(flow string) bool {
	return strings.Contains(flow, arpFlowsFingerprint)
}

func isTunneling(flow string) bool {
	return strings.Contains(flow, "in_port")
}

func parseGetFlowsOutput(output string) []string {
	flowsLines := strings.Split(output, "\n")
	flows := make([]string, 0, len(flowsLines))
	for _, aFlow := range flowsLines {
		flows = append(flows, string(aFlow))
	}
	return flows
}

// useful means the flow is not a tunneling flow
func (f *ovsFabric) usefulLocalFlowOfport(flow string) uint16 {
	ofportStr := f.arpOrDlTrafficFlowOfport(flow)
	ofport64, _ := strconv.ParseUint(ofportStr, 10, 16)
	return uint16(ofport64)
}

func (f *ovsFabric) arpOrDlTrafficFlowOfport(flow string) string {
	return f.flowParsingKit.decNbr.FindString(f.flowParsingKit.output.FindString(flow))
}

func (f *ovsFabric) parseLocalFlowPair(flowsPair []string) *LocalNetIfc {
	ifc := &LocalNetIfc{}

	// both flows in a pair store the vni, we can take it from the first
	// flow without checking its kind
	ifc.VNI = f.extractVNI(flowsPair[0])

	for _, aFlow := range flowsPair {
		if isARP(aFlow) {
			ifc.GuestIP = f.extractGuestIP(aFlow)
		} else {
			ifc.GuestMAC = f.extractGuestMAC(aFlow)
		}
	}

	return ifc
}

func (f *ovsFabric) parseRemoteFlowPair(flowsPair []string) RemoteNetIfc {
	ifc := RemoteNetIfc{}

	// VNI and host IP of a remote interface are stored in both flows created
	// for the interface, thus we can take them from the first flow of the pair
	// without knowing which one it is
	ifc.VNI = f.extractVNI(flowsPair[0])
	ifc.HostIP = f.extractHostIP(flowsPair[0])

	for _, aFlow := range flowsPair {
		if isARP(aFlow) {
			// only the ARP flow stores the Guest IP of the remote interface
			ifc.GuestIP = f.extractGuestIP(aFlow)
		} else {
			// only the Datalink traffifc flow stores the MAC of the interface
			ifc.GuestMAC = f.extractGuestMAC(aFlow)
		}
	}

	return ifc
}

func (f *ovsFabric) extractVNI(flow string) uint32 {
	// flows this method is invoked on (all but tunneling ones) store the vni as
	// a key value pair where the key is "tun_id" and the value is a hex number
	// with a leading "0x". To retrieve it we first get the key value pair with
	// key "tun_id", then we extract the value
	vniHex := f.flowParsingKit.hexNbr.FindString((f.flowParsingKit.tunID.FindString(flow)))
	vni, _ := strconv.ParseUint(strings.TrimLeft(vniHex, hexPrefixChars), 16, 32)
	return uint32(vni)
}

func (f *ovsFabric) extractHostIP(flow string) net.IP {
	// remote flows store the host IP by loading the hex representation of the
	// IP with the load instruction. To retrieve it we first get the load action
	// of the flow and then extract the value from it
	ipHex := f.flowParsingKit.hexNbr.FindString(f.flowParsingKit.load.FindString(flow))
	return hexStrToIPv4(ipHex)
}

func (f *ovsFabric) extractGuestIP(flow string) net.IP {
	// flows store the guest IP as a key value pair where the key is "arp_tpa".
	// get the key value pair, and then extract the IP from it
	guestIP := f.flowParsingKit.ipv4.FindString(f.flowParsingKit.arpTPA.FindString(flow))
	return net.ParseIP(guestIP)
}

func (f *ovsFabric) extractGuestMAC(flow string) net.HardwareAddr {
	// all the flows which store the guest MAC address of an interface store
	// only that MAC address, hence we can directly look for a string matching a
	// MAC address in the flow
	return net.HardwareAddr(f.flowParsingKit.mac.FindString(flow))
}

func (f *ovsFabric) extractCookie(flow string) string {
	return f.flowParsingKit.hexNbr.FindString(f.flowParsingKit.cookie.FindString(flow))
}

func hexStrToIPv4(hexStr string) net.IP {
	i64, _ := strconv.ParseUint(strings.TrimLeft(hexStr, hexPrefixChars), 16, 32)
	i := uint32(i64)
	return net.IPv4(uint8(i>>24), uint8(i>>16), uint8(i>>8), uint8(i))
}

func (f *ovsFabric) newCreateBridgeCmd() *exec.Cmd {
	// enable most OpenFlow protocols because each one has some commands useful
	// for manual inspection of the bridge and its flows. We need at least v1.5
	// because it's the first one to support bundling flows in a single transaction
	// TODO do something better than just hardcoding all the protocols
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
		f.vtep,
		"--",
		"set",
		"interface",
		f.vtep,
		"type=vxlan",
		"options:key=flow",
		"options:remote_ip=flow")
}

func (f *ovsFabric) newGetIfcOfportCmd(ifc string) *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"get",
		"interface",
		ifc,
		"ofport",
	)
}

func (f *ovsFabric) newCreateIfcCmd(ifc string, mac net.HardwareAddr) *exec.Cmd {
	// TODO think thoroughly if we need more than just the --may-exist flag
	// (e.g. what happens if the interface MAC changed?)
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
	// the --bundle flag makes the addition of the flows transactional, but it
	// works only if the flows are in a file
	cmd := exec.Command("ovs-ofctl", "--bundle", "add-flows", f.bridge, "-")
	cmd.Stdin = strings.NewReader(strings.Join(flows, "\n") + "\n")
	return cmd
}

func (f *ovsFabric) newDelFlowsCmd(flows ...string) *exec.Cmd {
	// the --bundle flag makes the deletion of the flows transactional, but it
	// works only if the flows are in a file
	cmd := exec.Command("ovs-ofctl", "--bundle", "del-flows", f.bridge, "-")
	cmd.Stdin = strings.NewReader(strings.Join(flows, "\n") + "\n")
	return cmd
}

func (f *ovsFabric) newGetFlowsCmd() *exec.Cmd {
	// TODO maybe we can do better: some options might filter out the flows we
	// do not need
	return exec.Command("ovs-ofctl", "dump-flows", f.bridge)
}

func (f *ovsFabric) newListOfportsAndIfcNamesCmd() *exec.Cmd {
	return exec.Command("ovs-vsctl",
		"-f",
		"table",
		"--no-heading",
		"--",
		"--columns=ofport,name",
		"list",
		"Interface")
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
