# Example: K8S-based OVS SDN

This example shows how to build a simple Software-Defined-Networking
(SDN) control plane using Kubernetes API machinery to build the
control plane and Kubernetes itself to host the control plane.

The data plan is based on OVS.  Each node has an independent OVS
installation.  The kube-based control plane distributes the needed
information to each node.

## Status

This is an early work in progress.  Currently there are initial drafts
of the kube API objects, the API extension server that holds them, the
interface to the local network machinery, and the controller that
assigns IP addresses.

## Architecture

This example augments a Kubernetes cluster with virtual networks as a
kind of workload.  These virtual networks are _not_ used for the
kubernetes networking.  This example can be added on to any Kubernetes
cluster, with any networking solution, as long as the nodes have the
`openvswitch-switch` package installed and can communicate with each
other using their `InternalIP` addresses.

This example adds the following to the Kubernetes cluster.

- An API extension server Deployment, with 1 replica, with a pod
  template that runs two containers:

-- the API extension server container that serves the custom resources
   used in this example, and
-- an etcd server dedicated to this example.

- A Deployment of a controller that assigns IP addresses.

- A DaemonSet of network agents that implement the node-local
  functionality of the SDN, by talking to the kube API machinery and
  the local networking implementation.

- A local networking implementation that has no control plane
  relationship with its counterpart on any other node.  This is a code
  library that has a golang interface and is compiled into the network
  agent.

## The SDN

This example creates a simple SDN.  It is deliberately simple, so that
this example can be relatively easy to read and understand.  The SDN
has just enough complexity to show some interesting issues.  The
implementation is intended to be efficient and reliable at scale.
(The current extension server Deployment is just an initial
development milestone; it will grow to have multiple extension servers
and multiple etcd servers.)

This SDN is not connected to anything.  It is not used by a CNI
plugin.  It is not used by any virtual machines.  It is a simple
stand-alone SDN.  It creates Linux network interfaces, but does not
connect them to anything.  It does not even implement DHCP nor
`dhclient` those network interfaces nor bind their IP addresses in any
other way.

The SDN implements virtual networks, using VXLAN encapsulation.  This
SDN does not attempt to support broadcast nor multicast traffic.  This
SDN avoids the problem of unicast to an unknown address by eagerly
disseminating the needed guestIP -> hostIP mapping information.

An SDN with as simple an API as this one could conceivably use a lazy
dissemination scheme for the guestIP -> hostIP mapping.  However, that
would conflict with the goals of this SDN in one or two ways.  A lazy
dissemination scheme would be more complex, and might not have one of
the key problems that this example is intended to illustrate --- while
a more realistic SDN _would_ have this eager dissemination problem
(e.g., for the ipset of a security group).

The SDN has just four concepts, as follows.

- VNI, a VXLAN virtual network identifier.  Usually represented by a
  golang `uint32`.  VNIs are chosen by the clients that create
  Subnets.

- Subnet, a kube custom resource.  A subnet has a single CIDR block,
  from which addresses are assigned (with the usual omissions).  Each
  Subnet is on a VNI, and a VNI can have multiple Subnets.

- NetworkAttachment, a kube custom resource.  A NetworkAttachment is a
  request and a response to that request for a Linux network
  interface attached to a particular Subnet on a particular node.  A
  NetworkAttachment has an IP address and a MAC address that are
  chosen by the SDN.

- IPLock, a kube custom resource.  An IPLock is the hard state that
  stipulates the usage of a particular IP address for a particular
  NetworkAttachment.

A controller chooses the IP address for each NetworkAttachment.  An
attachment's MAC address is a function of the IP address and the VNI.

The problems that the SDN solves are as follows.

- IP addresses must be assigned to the NetworkAttachments.  For a
  given VNI, a given IP address can be assigned to at most one
  NetworkAttachment at a time.

- For a given VNI, the relevant information about all of the VNI's
  NetworkAttachments must be communicated to every node that has a
  NetworkAttachment to that VNI; communicating this information to
  additional nodes is a waste to be avoided.

## The IPAM Controller

This is a singleton that assigns IP addresses to NetworkAttachments.

## The Network Agent

This is a controller that runs on each node and tells the local
networking implementation what it needs to know.

The most interesting problem faced by the network agent is how to stay
informed about all the relevant NetworkAttachment objects and none of
the irrelevant ones.  A NetworkAttachment object X is relevant to node
N if and only if there exists a NetworkAttachment object Y such that
X and Y have the same VNI and Y is on node N.

The following solutions were considered, with the last one adopted.

- A network agent has one Informer whose list&watch get all
  NetworkAttachment objects from the apiservers and filtering is done
  on the client side.  This was rejected because it can impose a LOT
  more load on the apiservers than necessary.

- A network agent has one Informer whose list&watch filter based on
  presence of a label specific to the agent's node, and there is a
  controller that adds the needed labels to the NetworkAttachment
  objects.  This was rejected because it involves a LOT of additional
  work to manage the labels.

- A network agent has, at any given time, one Informer whose
  list&watch filter based on testing whether the NetworkAttachment's
  VNI value is in the set of currently relevant VNIs.  Whenever that
  set changes, the old Informer is stopped and a new one is created.
  This was rejected because the new Informer's initial list operation
  will produce largely redundant information, causing unnecessary load
  on the agents and the apiservers.

- A network agent has, at any given time when the number of relevant
  VNIs is R, 1+R Informers on NetworkAttachments.  For one of those
  Informers, list&watch filter on whether `spec.node` identifies the
  agent's node.  Each of the other Informers is specific to a VNI, and
  that Informer's list&watch filter on whether `spec.vni` equals the
  Informer's VNI.

A network agent can and shold be as selective regarding Subnets as
regarding NetworkAttachments.
