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

  - the API extension server container that serves the custom resources
    used in this example, and

  - an etcd server dedicated to this example.

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
disseminating the needed guestIP -> hostIP mappings to the possible
senders.

An SDN with as simple an API as this one could conceivably use a lazy
dissemination scheme for the guestIP -> hostIP mappings.  However,
that would conflict with the goals of this SDN in one or two ways.  A
lazy dissemination scheme would be more complex, and might not have
one of the key problems that this example is intended to illustrate
--- while a more realistic SDN _would_ have this eager dissemination
problem (e.g., for the ipset of a security group).

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

- For a given VNI, the guestIP->hostIP mapping for each of the VNI's
  NetworkAttachments must be communicated to every node that has a
  NetworkAttachment to that VNI --- and should not be communicated to
  other nodes (because that communication would impose unnecessary
  costs).

## The IPAM Controller

This is a singleton that assigns IP addresses to NetworkAttachments.
It is managed by a Deployment object with its number of replicas set
to 1.

The following three approaches were considered, with the last one
taken.

One approach would be to make the IP address controller keep itself
appraised of all IP address assignments and assign unused addresses
when needed based on an in-memory cache of all the assigned addresses.
This can be correct only if it is impossible for there to be
two such processes running at once.  Kubernetes does not actually make
such a guarantee.  Although Kubernetes includes mechanisms to detect
failures and recover from them, no failure detector is perfect.

Another approach would be to keep a bitmap in API objects indicating
which addresses have been taken.  Not one bitmap --- that would be too
big --- but some suitable structure that holds one bit per "possible"
address.  The problem with this approach is that the Kubernetes API
machinery does not support any ACID transaction that involves more
than one object.  It is impossible to simultaneously update that
allocation table _and_ a NetworkAttachment.  The address controller
would have to update one first and then the other.  If the controller
crashes in between those two operations then an address will either be
used without being marked as taken or be lost to all future use.

The approach taken is to use the Kubernetes API machinery to construct
locks on IP addresses.  A lock is implemented by an API object whose
name is a function of the VNI and the IP address.  Attempting to take
a lock amounts to trying to create the IP lock object; creation of the
object equals successfully taking the lock.  A lock object's
`ObjectMeta.OwnerReferences` include a reference to the
NetworkAttachment that holds the lock.

This controller does not enforce or assume any connections between the
lifecycles of NetworkAttachments and their Subnets; either can be
created or deleted at any time.  There are, however, some intended
consistency constraints between certain fields of certain objects.
The apiservers attempt to enforce this consistency, but this
enforcement is necessarily imperfect because there are no ACID
transactions that involve more than one object.  The controllers
therefore must be prepared to react safely if and when an
inconsistency gets through.

As with any controller, one of the IP address controller's problems is
how to avoid doing duplicate work while waiting for its earlier
actions to fully take effect.  To save on client/server traffic, this
controller does not actively query the apiservers to find out its
previous actions; rather, this controller simply waits to be informed
through its Informers.  In the interim, this conrtroller maintains a
record of actions in flight.  In particular, for each address
assignment in flight, the controller records: the ResourceVersion of
the NetworkAttachment that was seen to need an IP address, the
ResourceVersion of the Subnet that was referenced when making the
assignment, the IP address assigned, and the ResourceVersion of the
NetworkAttachment created by the update that writes the assigned
address into the status of the attachment object.  As long as the
Subnet's ResourceVersion is unchanged and the attachment object's
ResourceVersion is one of the two recorded, the record is valid and
retained.

## The Network Agent

This is a controller that runs on each node and tells the local
networking implementation what it needs to know.

The most interesting problem faced by the network agent is how to stay
informed about all the relevant NetworkAttachment objects and none of
the irrelevant ones.  A NetworkAttachment object X is relevant to node
N if and only if there exists a NetworkAttachment object Y such that
X and Y have the same VNI and Y is on node N.

The following approaches were considered, with the last one adopted.

- A network agent has one Informer whose list&watch get all
  NetworkAttachment objects from the apiservers and filtering is done
  on the client side.  This was rejected because it can impose a LOT
  more load on the apiservers and agents than necessary.

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
  that Informer's list&watch filter on whether `spec.vni` (yes, that's
  denormalized data) equals the Informer's VNI.

A network agent can and should be as selective regarding Subnets as
regarding NetworkAttachments.