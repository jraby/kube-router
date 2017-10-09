package controllers

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudnativelabs/kube-router/app/options"
	"github.com/cloudnativelabs/kube-router/app/watchers"
	"github.com/cloudnativelabs/kube-router/utils"
	"github.com/coreos/go-iptables/iptables"
	"github.com/golang/glog"
	api "k8s.io/api/core/v1"
	apiextensions "k8s.io/api/extensions/v1beta1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/client-go/kubernetes"
)

// Network policy controller provides both ingress and egress filtering for the pods as per the defined network
// policies. Two different types of iptables chains are used. Each pod running on the node which either
// requires ingress or egress filtering gets a pod specific chian. Each network policy has a iptable chain, which
// has rules expreessed through ipsets matching source and destination pod ip's. In the FORWARD chain of the
// filter table a rule is added to jump the traffic originating (in case of egress network policy) from the pod
// or destined (in case of ingress network policy) to the pod to the pod specific iptable chain. Each
// pod specifc iptable chain has rules to jump to the network polices chains, that pod matches. So packet
// originating/destined from/to pod goes throuh fitler table's, FORWARD chain, followed by pod specific chain,
// followed by one or more network policy chains, till there is a match which will accept the packet, or gets
// dropped by the rule in the pod chain, if there is no match.

// NetworkPolicyController strcut to hold information required by NetworkPolicyController
type NetworkPolicyController struct {
	nodeIP          net.IP
	nodeHostName    string
	mu              sync.Mutex
	syncPeriod      time.Duration
	v1NetworkPolicy bool

	// list of all active network policies expressed as networkPolicyInfo
	networkPoliciesInfo *[]networkPolicyInfo
	ipSetHandler        *utils.IPSet
}

// internal structure to represent a network policy
type networkPolicyInfo struct {
	name      string
	namespace string
	labels    map[string]string

	// set of pods matching network policy spec podselector label selector
	targetPods map[string]podInfo

	// whitelist ingress rules from the network policy spec
	ingressRules []ingressRule

	// whitelist egress rules from the network policy spec
	egressRules []egressRule

	// policy type "ingress" or "egress" or "both" as defined by PolicyType in the spec
	policyType string
}

// internal structure to represent Pod
type podInfo struct {
	ip        string
	name      string
	namespace string
	labels    map[string]string
}

// internal stucture to represent NetworkPolicyIngressRule in the spec
type ingressRule struct {
	matchAllPorts  bool
	ports          []protocolAndPort
	matchAllSource bool
	srcPods        []podInfo
}

// internal structure to represent NetworkPolicyEgressRule in the spec
type egressRule struct {
	matchAllPorts        bool
	ports                []protocolAndPort
	matchAllDestinations bool
	dstPods              []podInfo
}

type protocolAndPort struct {
	protocol string
	port     string
}

// Run runs forver till we receive notification on stopCh
func (npc *NetworkPolicyController) Run(stopCh <-chan struct{}, wg *sync.WaitGroup) {
	t := time.NewTicker(npc.syncPeriod)
	defer t.Stop()
	defer wg.Done()

	glog.Infof("Starting network policy controller")

	// loop forever till notified to stop on stopCh
	for {
		select {
		case <-stopCh:
			glog.Infof("Shutting down network policies controller")
			return
		default:
		}

		if watchers.PodWatcher.HasSynced() && watchers.NetworkPolicyWatcher.HasSynced() {
			glog.Infof("Performing periodic syn of the iptables to reflect network policies")
			err := npc.Sync()
			if err != nil {
				glog.Errorf("Error during periodic sync: " + err.Error())
			}
		} else {
			continue
		}

		select {
		case <-stopCh:
			glog.Infof("Shutting down network policies controller")
			return
		case <-t.C:
		}
	}
}

// OnPodUpdate handles updates to pods from the Kubernetes api server
func (npc *NetworkPolicyController) OnPodUpdate(podUpdate *watchers.PodUpdate) {
	glog.Infof("Received pod update namspace:%s pod name:%s", podUpdate.Pod.Namespace, podUpdate.Pod.Name)
	if watchers.PodWatcher.HasSynced() && watchers.NetworkPolicyWatcher.HasSynced() {
		err := npc.Sync()
		if err != nil {
			glog.Errorf("Error syncing on pod update: %s", err)
		}
	} else {
		glog.Infof("Received pod update, but controller not in sync")
	}
}

// OnNetworkPolicyUpdate handles updates to network policy from the kubernetes api server
func (npc *NetworkPolicyController) OnNetworkPolicyUpdate(networkPolicyUpdate *watchers.NetworkPolicyUpdate) {
	if watchers.PodWatcher.HasSynced() && watchers.NetworkPolicyWatcher.HasSynced() {
		err := npc.Sync()
		if err != nil {
			glog.Errorf("Error syncing on network policy update: %s", err)
		}
	} else {
		glog.Infof("Received network policy update, but controller not in sync")
	}
}

// OnNamespaceUpdate handles updates to namespace from kubernetes api server
func (npc *NetworkPolicyController) OnNamespaceUpdate(namespaceUpdate *watchers.NamespaceUpdate) {

	// namespace (and annotations on it) has no significance in GA ver of network policy
	if npc.v1NetworkPolicy {
		return
	}

	glog.Infof("Received namesapce update namspace:%s", namespaceUpdate.Namespace.Name)
	if watchers.PodWatcher.HasSynced() && watchers.NetworkPolicyWatcher.HasSynced() {
		err := npc.Sync()
		if err != nil {
			glog.Errorf("Error syncing on namespace update: %s", err)
		}
	} else {
		glog.Infof("Received namspace update, but controller not in sync")
	}
}

// Sync synchronizes iptables to desired state of network policies
func (npc *NetworkPolicyController) Sync() error {

	var err error
	npc.mu.Lock()
	defer npc.mu.Unlock()

	start := time.Now()
	defer func() {
		glog.Infof("sync iptables took %v", time.Since(start))
	}()

	if npc.v1NetworkPolicy {
		npc.networkPoliciesInfo, err = buildNetworkPoliciesInfo()
		if err != nil {
			return errors.New("Aborting sync. Failed to build network policies: " + err.Error())
		}
	} else {
		// TODO remove the Beta support
		npc.networkPoliciesInfo, err = buildBetaNetworkPoliciesInfo()
		if err != nil {
			return errors.New("Aborting sync. Failed to build network policies: " + err.Error())
		}
	}

	activePolicyChains, activePolicyIpSets, err := npc.syncNetworkPolicyChains()
	if err != nil {
		return errors.New("Aborting sync. Failed to sync network policy chains: " + err.Error())
	}

	activePodFwChains, err := npc.syncPodFirewallChains()
	if err != nil {
		return errors.New("Aborting sync. Failed to sync pod firewalls: " + err.Error())
	}

	err = cleanupStaleRules(activePolicyChains, activePodFwChains, activePolicyIpSets)
	if err != nil {
		return errors.New("Aborting sync. Failed to cleanup stale iptable rules: " + err.Error())
	}

	return nil
}

// Configure iptable rules representing each network policy. All pod's matched by
// network policy spec podselector labels are grouped together in one ipset which
// is used for matching destination ip address. Each ingress rule in the network
// policyspec is evaluated to set of matching pods, which are grouped in to a
// ipset used for source ip addr matching.
func (npc *NetworkPolicyController) syncNetworkPolicyChains() (map[string]bool, map[string]bool, error) {

	activePolicyChains := make(map[string]bool)
	activePolicyIpSets := make(map[string]bool)

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor due to: %s", err.Error())
	}

	// run through all network policies
	for _, policy := range *npc.networkPoliciesInfo {

		// ensure there is a unique chain per network policy in filter table
		policyChainName := networkPolicyChainName(policy.namespace, policy.name)
		err := iptablesCmdHandler.NewChain("filter", policyChainName)
		if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
			return nil, nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}

		activePolicyChains[policyChainName] = true

		// create a ipset for all destination pod ip's matched by the policy spec PodSelector
		targetDestPodIpSetName := policyDestinationPodIpSetName(policy.namespace, policy.name)
		targetDestPodIpSet, err := npc.ipSetHandler.Create(targetDestPodIpSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create ipset: %s", err.Error())
		}

		// create a ipset for all source pod ip's matched by the policy spec PodSelector
		targetSourcePodIpSetName := policySourcePodIpSetName(policy.namespace, policy.name)
		targetSourcePodIpSet, err := npc.ipSetHandler.Create(targetSourcePodIpSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create ipset: %s", err.Error())
		}

		// flush all entries in the set
		if targetSourcePodIpSet.Flush() != nil {
			return nil, nil, fmt.Errorf("failed to flush ipset while syncing iptables: %s", err.Error())
		}
		if targetDestPodIpSet.Flush() != nil {
			return nil, nil, fmt.Errorf("failed to flush ipset while syncing iptables: %s", err.Error())
		}

		activePolicyIpSets[targetDestPodIpSet.Name] = true
		activePolicyIpSets[targetSourcePodIpSet.Name] = true

		for k := range policy.targetPods {
			// TODO restrict ipset to ip's of pods running on the node
			targetDestPodIpSet.Add(k, utils.OptionTimeout, "0")
			targetSourcePodIpSet.Add(k, utils.OptionTimeout, "0")
		}

		// TODO use iptables-restore to better implement the logic, than flush and add rules
		err = iptablesCmdHandler.ClearChain("filter", policyChainName)
		if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
			return nil, nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}

		err = npc.processIngressRules(policy, targetDestPodIpSetName, activePolicyIpSets)
		if err != nil {
			return nil, nil, err
		}

		err = npc.processEgressRules(policy, targetSourcePodIpSetName, activePolicyIpSets)
		if err != nil {
			return nil, nil, err
		}
	}

	glog.Infof("Iptables chains in the filter table are synchronized with the network policies.")

	return activePolicyChains, activePolicyIpSets, nil
}

func (npc *NetworkPolicyController) processIngressRules(policy networkPolicyInfo,
	targetDestPodIpSetName string, activePolicyIpSets map[string]bool) error {

	// From network policy spec: "If field 'Ingress' is empty then this NetworkPolicy does not allow any traffic "
	// so no whitelist rules to be added to the network policy
	if policy.ingressRules == nil {
		return nil
	}

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		return fmt.Errorf("Failed to initialize iptables executor due to: %s", err.Error())
	}

	policyChainName := networkPolicyChainName(policy.namespace, policy.name)

	// run through all the ingress rules in the spec and create iptable rules
	// in the chain for the network policy
	for i, ingressRule := range policy.ingressRules {

		if len(ingressRule.srcPods) != 0 {
			srcPodIpSetName := policyIndexedSourcePodIpSetName(policy.namespace, policy.name, i)
			srcPodIpSet, err := npc.ipSetHandler.Create(srcPodIpSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
			if err != nil {
				return fmt.Errorf("failed to create ipset: %s", err.Error())
			}
			// flush all entries in the set
			if srcPodIpSet.Flush() != nil {
				return fmt.Errorf("failed to flush ipset while syncing iptables: %s", err.Error())
			}

			activePolicyIpSets[srcPodIpSet.Name] = true

			for _, pod := range ingressRule.srcPods {
				srcPodIpSet.Add(pod.ip, utils.OptionTimeout, "0")
			}

			if len(ingressRule.ports) != 0 {
				// case where 'ports' details and 'from' details specified in the ingress rule
				// so match on specified source and destination ip's and specified port and protocol
				for _, portProtocol := range ingressRule.ports {
					comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name " +
						policy.name + " namespace " + policy.namespace
					args := []string{"-m", "comment", "--comment", comment,
						"-m", "set", "--set", srcPodIpSetName, "src",
						"-m", "set", "--set", targetDestPodIpSetName, "dst",
						"-p", portProtocol.protocol,
						"--dport", portProtocol.port,
						"-j", "ACCEPT"}
					err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
					if err != nil {
						return fmt.Errorf("Failed to run iptables command: %s", err.Error())
					}
				}
			} else {
				// case where no 'ports' details specified in the ingress rule but 'from' details specified
				// so match on specified source and destination ip with all port and protocol
				comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name " +
					policy.name + " namespace " + policy.namespace
				args := []string{"-m", "comment", "--comment", comment,
					"-m", "set", "--set", srcPodIpSetName, "src",
					"-m", "set", "--set", targetDestPodIpSetName, "dst",
					"-j", "ACCEPT"}
				err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
				if err != nil {
					return fmt.Errorf("Failed to run iptables command: %s", err.Error())
				}
			}
		}

		// case where only 'ports' details specified but no 'from' details in the ingress rule
		// so match on all sources, with specified port and protocol
		if ingressRule.matchAllSource && !ingressRule.matchAllPorts {
			for _, portProtocol := range ingressRule.ports {
				comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name: " +
					policy.name + " namespace " + policy.namespace
				args := []string{"-m", "comment", "--comment", comment,
					"-m", "set", "--set", targetDestPodIpSetName, "dst",
					"-p", portProtocol.protocol,
					"--dport", portProtocol.port,
					"-j", "ACCEPT"}
				err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
				if err != nil {
					return fmt.Errorf("Failed to run iptables command: %s", err.Error())
				}
			}
		}

		// case where nether ports nor from details are speified in the ingress rule
		// so match on all ports, protocol, source IP's
		if ingressRule.matchAllSource && ingressRule.matchAllPorts {
			comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name: " +
				policy.name + " namespace " + policy.namespace
			args := []string{"-m", "comment", "--comment", comment,
				"-m", "set", "--set", targetDestPodIpSetName, "dst",
				"-j", "ACCEPT"}
			err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
			if err != nil {
				return fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	return nil
}

func (npc *NetworkPolicyController) processEgressRules(policy networkPolicyInfo,
	targetSourcePodIpSetName string, activePolicyIpSets map[string]bool) error {

	// From network policy spec: "If field 'Ingress' is empty then this NetworkPolicy does not allow any traffic "
	// so no whitelist rules to be added to the network policy
	if policy.egressRules == nil {
		return nil
	}

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		return fmt.Errorf("Failed to initialize iptables executor due to: %s", err.Error())
	}

	policyChainName := networkPolicyChainName(policy.namespace, policy.name)

	// run through all the egress rules in the spec and create iptable rules
	// in the chain for the network policy
	for i, egressRule := range policy.egressRules {

		if len(egressRule.dstPods) != 0 {
			dstPodIpSetName := policyIndexedDestinationPodIpSetName(policy.namespace, policy.name, i)
			dstPodIpSet, err := npc.ipSetHandler.Create(dstPodIpSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
			if err != nil {
				return fmt.Errorf("failed to create ipset: %s", err.Error())
			}
			// flush all entries in the set
			if dstPodIpSet.Flush() != nil {
				return fmt.Errorf("failed to flush ipset while syncing iptables: %s", err.Error())
			}

			activePolicyIpSets[dstPodIpSet.Name] = true

			for _, pod := range egressRule.dstPods {
				dstPodIpSet.Add(pod.ip, utils.OptionTimeout, "0")
			}

			if len(egressRule.ports) != 0 {
				// case where 'ports' details and 'from' details specified in the egress rule
				// so match on specified source and destination ip's and specified port and protocol
				for _, portProtocol := range egressRule.ports {
					comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name " +
						policy.name + " namespace " + policy.namespace
					args := []string{"-m", "comment", "--comment", comment,
						"-m", "set", "--set", targetSourcePodIpSetName, "src",
						"-m", "set", "--set", dstPodIpSetName, "dst",
						"-p", portProtocol.protocol,
						"--dport", portProtocol.port,
						"-j", "ACCEPT"}
					err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
					if err != nil {
						return fmt.Errorf("Failed to run iptables command: %s", err.Error())
					}
				}
			} else {
				// case where no 'ports' details specified in the ingress rule but 'from' details specified
				// so match on specified source and destination ip with all port and protocol
				comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name " +
					policy.name + " namespace " + policy.namespace
				args := []string{"-m", "comment", "--comment", comment,
					"-m", "set", "--set", targetSourcePodIpSetName, "src",
					"-m", "set", "--set", dstPodIpSetName, "dst",
					"-j", "ACCEPT"}
				err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
				if err != nil {
					return fmt.Errorf("Failed to run iptables command: %s", err.Error())
				}
			}
		}

		// case where only 'ports' details specified but no 'to' details in the egress rule
		// so match on all sources, with specified port and protocol
		if egressRule.matchAllDestinations && !egressRule.matchAllPorts {
			for _, portProtocol := range egressRule.ports {
				comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name: " +
					policy.name + " namespace " + policy.namespace
				args := []string{"-m", "comment", "--comment", comment,
					"-m", "set", "--set", targetSourcePodIpSetName, "src",
					"-p", portProtocol.protocol,
					"--dport", portProtocol.port,
					"-j", "ACCEPT"}
				err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
				if err != nil {
					return fmt.Errorf("Failed to run iptables command: %s", err.Error())
				}
			}
		}

		// case where nether ports nor from details are speified in the egress rule
		// so match on all ports, protocol, source IP's
		if egressRule.matchAllDestinations && egressRule.matchAllPorts {
			comment := "rule to ACCEPT traffic from source pods to dest pods selected by policy name: " +
				policy.name + " namespace " + policy.namespace
			args := []string{"-m", "comment", "--comment", comment,
				"-m", "set", "--set", targetSourcePodIpSetName, "src",
				"-j", "ACCEPT"}
			err := iptablesCmdHandler.AppendUnique("filter", policyChainName, args...)
			if err != nil {
				return fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	return nil
}

func (npc *NetworkPolicyController) syncPodFirewallChains() (map[string]bool, error) {

	activePodFwChains := make(map[string]bool)

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor: %s", err.Error())
	}

	// loop through the pods running on the node which to which ingress network policies to be applied
	ingressNetworkPolicyEnabledPods, err := npc.getIngressNetworkPolicyEnabledPods(npc.nodeIP.String())
	if err != nil {
		return nil, err
	}
	for _, pod := range *ingressNetworkPolicyEnabledPods {

		// below condition occurs when we get trasient update while removing or adding pod
		// subseqent update will do the correct action
		if len(pod.ip) == 0 || pod.ip == "" {
			continue
		}

		// ensure pod specific firewall chain exist for all the pods that need ingress firewall
		podFwChainName := podFirewallChainName(pod.namespace, pod.name)
		err = iptablesCmdHandler.NewChain("filter", podFwChainName)
		if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		activePodFwChains[podFwChainName] = true

		// ensure there is rule in filter table and FORWARD chain to jump to pod specific firewall chain
		// this rule applies to the traffic getting routed (coming for other node pods)
		comment := "rule to jump traffic destined to POD name:" + pod.name + " namespace: " + pod.namespace +
			" to chain " + podFwChainName
		args := []string{"-m", "comment", "--comment", comment, "-d", pod.ip, "-j", podFwChainName}
		exists, err := iptablesCmdHandler.Exists("filter", "FORWARD", args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", "FORWARD", 1, args...)
			if err != nil {
				return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}

		// ensure there is rule in filter table and OUTPUT chain to jump to pod specific firewall chain
		// this rule applies to the traffic from a pod getting routed back to another pod on same node by service proxy
		exists, err = iptablesCmdHandler.Exists("filter", "OUTPUT", args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", "OUTPUT", 1, args...)
			if err != nil {
				return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}

		// ensure there is rule in filter table and forward chain to jump to pod specific firewall chain
		// this rule applies to the traffic getting switched (coming for same node pods)
		comment = "rule to jump traffic destined to POD name:" + pod.name + " namespace: " + pod.namespace +
			" to chain " + podFwChainName
		args = []string{"-m", "physdev", "--physdev-is-bridged",
			"-m", "comment", "--comment", comment,
			"-d", pod.ip,
			"-j", podFwChainName}
		exists, err = iptablesCmdHandler.Exists("filter", "FORWARD", args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err = iptablesCmdHandler.Insert("filter", "FORWARD", 1, args...)
			if err != nil {
				return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}

		// add default DROP rule at the end of chain
		comment = "default rule to REJECT traffic destined for POD name:" + pod.name + " namespace: " + pod.namespace
		args = []string{"-m", "comment", "--comment", comment, "-j", "REJECT"}
		err = iptablesCmdHandler.AppendUnique("filter", podFwChainName, args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}

		// add entries in pod firewall to run through required network policies
		for _, policy := range *npc.networkPoliciesInfo {
			if _, ok := policy.targetPods[pod.ip]; ok {
				comment := "run through nw policy " + policy.name
				policyChainName := networkPolicyChainName(policy.namespace, policy.name)
				args := []string{"-m", "comment", "--comment", comment, "-j", policyChainName}
				exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
				if err != nil {
					return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
				}
				if !exists {
					err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
					if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
						return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
					}
				}
			}
		}

		// ensure statefull firewall, that permits return traffic for the traffic originated by the pod
		comment = "rule for stateful firewall for pod"
		args = []string{"-m", "comment", "--comment", comment, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
		exists, err = iptablesCmdHandler.Exists("filter", podFwChainName, args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
			if err != nil {
				return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	// loop through the pods running on the node which egress network policies to be applied
	egressNetworkPolicyEnabledPods, err := npc.getEgressNetworkPolicyEnabledPods(npc.nodeIP.String())
	if err != nil {
		return nil, err
	}
	for _, pod := range *egressNetworkPolicyEnabledPods {

		// below condition occurs when we get trasient update while removing or adding pod
		// subseqent update will do the correct action
		if len(pod.ip) == 0 || pod.ip == "" {
			continue
		}

		// ensure pod specific firewall chain exist for all the pods that need egress firewall
		podFwChainName := podFirewallChainName(pod.namespace, pod.name)
		err = iptablesCmdHandler.NewChain("filter", podFwChainName)
		if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		activePodFwChains[podFwChainName] = true

		// ensure there is rule in filter table and FORWARD chain to jump to pod specific firewall chain
		// this rule applies to the traffic getting routed (coming for other node pods)
		comment := "rule to jump traffic from POD name:" + pod.name + " namespace: " + pod.namespace +
			" to chain " + podFwChainName
		args := []string{"-m", "comment", "--comment", comment, "-s", pod.ip, "-j", podFwChainName}
		exists, err := iptablesCmdHandler.Exists("filter", "FORWARD", args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", "FORWARD", 1, args...)
			if err != nil {
				return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}

		// ensure there is rule in filter table and forward chain to jump to pod specific firewall chain
		// this rule applies to the traffic getting switched (coming for same node pods)
		comment = "rule to jump traffic from POD name:" + pod.name + " namespace: " + pod.namespace +
			" to chain " + podFwChainName
		args = []string{"-m", "physdev", "--physdev-is-bridged",
			"-m", "comment", "--comment", comment,
			"-s", pod.ip,
			"-j", podFwChainName}
		exists, err = iptablesCmdHandler.Exists("filter", "FORWARD", args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err = iptablesCmdHandler.Insert("filter", "FORWARD", 1, args...)
			if err != nil {
				return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}

		// add default DROP rule at the end of chain
		comment = "default rule to REJECT traffic destined for POD name:" + pod.name + " namespace: " + pod.namespace
		args = []string{"-m", "comment", "--comment", comment, "-j", "REJECT"}
		err = iptablesCmdHandler.AppendUnique("filter", podFwChainName, args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}

		// add entries in pod firewall to run through required network policies
		for _, policy := range *npc.networkPoliciesInfo {
			if _, ok := policy.targetPods[pod.ip]; ok {
				comment := "run through nw policy " + policy.name
				policyChainName := networkPolicyChainName(policy.namespace, policy.name)
				args := []string{"-m", "comment", "--comment", comment, "-j", policyChainName}
				exists, err := iptablesCmdHandler.Exists("filter", podFwChainName, args...)
				if err != nil {
					return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
				}
				if !exists {
					err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
					if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
						return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
					}
				}
			}
		}

		// ensure statefull firewall, that permits return traffic for the traffic originated by the pod
		comment = "rule for stateful firewall for pod"
		args = []string{"-m", "comment", "--comment", comment, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
		exists, err = iptablesCmdHandler.Exists("filter", podFwChainName, args...)
		if err != nil {
			return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", podFwChainName, 1, args...)
			if err != nil {
				return nil, fmt.Errorf("Failed to run iptables command: %s", err.Error())
			}
		}
	}

	return activePodFwChains, nil
}

func cleanupStaleRules(activePolicyChains, activePodFwChains, activePolicyIPSets map[string]bool) error {

	cleanupPodFwChains := make([]string, 0)
	cleanupPolicyChains := make([]string, 0)
	cleanupPolicyIPSets := make([]*utils.Set, 0)

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("failed to initialize iptables command executor due to %s", err.Error())
	}
	ipsets, err := utils.NewIPSet()
	if err != nil {
		glog.Fatalf("failed to create ipsets command executor due to %s", err.Error())
	}
	err = ipsets.Save()
	if err != nil {
		glog.Fatalf("failed to initialize ipsets command executor due to %s", err.Error())
	}

	// get the list of chains created for pod firewall and network policies
	chains, err := iptablesCmdHandler.ListChains("filter")
	for _, chain := range chains {
		if strings.HasPrefix(chain, "KUBE-NWPLCY-") {
			if _, ok := activePolicyChains[chain]; !ok {
				cleanupPolicyChains = append(cleanupPolicyChains, chain)
			}
		}
		if strings.HasPrefix(chain, "KUBE-POD-FW-") {
			if _, ok := activePodFwChains[chain]; !ok {
				cleanupPodFwChains = append(cleanupPodFwChains, chain)
			}
		}
	}
	for _, set := range ipsets.Sets {
		if strings.HasPrefix(set.Name, "KUBE-SRC-") ||
			strings.HasPrefix(set.Name, "KUBE-DST-") {
			if _, ok := activePolicyIPSets[set.Name]; !ok {
				cleanupPolicyIPSets = append(cleanupPolicyIPSets, set)
			}
		}
	}

	// cleanup FORWARD chain rules to jump to pod firewall
	for _, chain := range cleanupPodFwChains {

		forwardChainRules, err := iptablesCmdHandler.List("filter", "FORWARD")
		if err != nil {
			return fmt.Errorf("failed to list rules in filter table, FORWARD chain due to %s", err.Error())
		}
		outputChainRules, err := iptablesCmdHandler.List("filter", "OUTPUT")
		if err != nil {
			return fmt.Errorf("failed to list rules in filter table, OUTPUT chain due to %s", err.Error())
		}

		// TODO delete rule by spec, than rule number to avoid extra loop
		var realRuleNo int
		for i, rule := range forwardChainRules {
			if strings.Contains(rule, chain) {
				err = iptablesCmdHandler.Delete("filter", "FORWARD", strconv.Itoa(i-realRuleNo))
				if err != nil {
					return fmt.Errorf("failed to delete rule: %s from the FORWARD chain of filter table due to %s", rule, err.Error())
				}
				realRuleNo++
			}
		}
		realRuleNo = 0
		for i, rule := range outputChainRules {
			if strings.Contains(rule, chain) {
				err = iptablesCmdHandler.Delete("filter", "OUTPUT", strconv.Itoa(i-realRuleNo))
				if err != nil {
					return fmt.Errorf("failed to delete rule: %s from the OUTPUT chain of filter table due to %s", rule, err.Error())
				}
				realRuleNo++
			}
		}
	}

	// cleanup pod firewall chain
	for _, chain := range cleanupPodFwChains {
		glog.Errorf("Found pod fw chain to cleanup: %s", chain)
		err = iptablesCmdHandler.ClearChain("filter", chain)
		if err != nil {
			return fmt.Errorf("Failed to flush the rules in chain %s due to %s", chain, err.Error())
		}
		err = iptablesCmdHandler.DeleteChain("filter", chain)
		if err != nil {
			return fmt.Errorf("Failed to delete the chain %s due to %s", chain, err.Error())
		}
		glog.Infof("Deleted pod specific firewall chain: %s from the filter table", chain)
	}

	// cleanup network policy chains
	for _, policyChain := range cleanupPolicyChains {
		glog.Infof("Found policy chain to cleanup %s", policyChain)

		// first clean up any references from pod firewall chain
		for podFwChain := range activePodFwChains {
			podFwChainRules, err := iptablesCmdHandler.List("filter", podFwChain)
			if err != nil {

			}
			for i, rule := range podFwChainRules {
				if strings.Contains(rule, policyChain) {
					err = iptablesCmdHandler.Delete("filter", podFwChain, strconv.Itoa(i))
					if err != nil {
						return fmt.Errorf("Failed to delete rule %s from the chain %s", rule, podFwChain)
					}
					break
				}
			}
		}

		err = iptablesCmdHandler.ClearChain("filter", policyChain)
		if err != nil {
			return fmt.Errorf("Failed to flush the rules in chain %s due to  %s", policyChain, err)
		}
		err = iptablesCmdHandler.DeleteChain("filter", policyChain)
		if err != nil {
			return fmt.Errorf("Failed to flush the rules in chain %s due to %s", policyChain, err)
		}
		glog.Infof("Deleted network policy chain: %s from the filter table", policyChain)
	}

	// cleanup network policy ipsets
	for _, set := range cleanupPolicyIPSets {
		err = set.Destroy()
		if err != nil {
			return fmt.Errorf("Failed to delete ipset %s due to %s", set.Name, err)
		}
	}
	return nil
}

func (npc *NetworkPolicyController) getIngressNetworkPolicyEnabledPods(nodeIp string) (*map[string]podInfo, error) {
	nodePods := make(map[string]podInfo)

	for _, pod := range watchers.PodWatcher.List() {
		if strings.Compare(pod.Status.HostIP, nodeIp) != 0 {
			continue
		}
		for _, policy := range *npc.networkPoliciesInfo {
			if policy.namespace != pod.ObjectMeta.Namespace {
				continue
			}
			_, ok := policy.targetPods[pod.Status.PodIP]
			if ok && (policy.policyType == "both" || policy.policyType == "ingress") {
				glog.Infof("Found pod name: " + pod.ObjectMeta.Name + " namespace: " + pod.ObjectMeta.Namespace + " for which network policies need to be applied.")
				nodePods[pod.Status.PodIP] = podInfo{ip: pod.Status.PodIP,
					name:      pod.ObjectMeta.Name,
					namespace: pod.ObjectMeta.Namespace,
					labels:    pod.ObjectMeta.Labels}
				break
			}
		}
	}
	return &nodePods, nil

}

func (npc *NetworkPolicyController) getEgressNetworkPolicyEnabledPods(nodeIp string) (*map[string]podInfo, error) {

	nodePods := make(map[string]podInfo)

	for _, pod := range watchers.PodWatcher.List() {
		if strings.Compare(pod.Status.HostIP, nodeIp) != 0 {
			continue
		}
		for _, policy := range *npc.networkPoliciesInfo {
			if policy.namespace != pod.ObjectMeta.Namespace {
				continue
			}
			_, ok := policy.targetPods[pod.Status.PodIP]
			if ok && (policy.policyType == "both" || policy.policyType == "egress") {
				glog.Infof("Found pod name: " + pod.ObjectMeta.Name + " namespace: " + pod.ObjectMeta.Namespace + " for which network policies need to be applied.")
				nodePods[pod.Status.PodIP] = podInfo{ip: pod.Status.PodIP,
					name:      pod.ObjectMeta.Name,
					namespace: pod.ObjectMeta.Namespace,
					labels:    pod.ObjectMeta.Labels}
				break
			}
		}
	}
	return &nodePods, nil
}

func buildNetworkPoliciesInfo() (*[]networkPolicyInfo, error) {

	NetworkPolicies := make([]networkPolicyInfo, 0)

	for _, policyObj := range watchers.NetworkPolicyWatcher.List() {

		policy, ok := policyObj.(*networking.NetworkPolicy)
		if !ok {
			return nil, fmt.Errorf("Failed to convert")
		}
		newPolicy := networkPolicyInfo{
			name:       policy.Name,
			namespace:  policy.Namespace,
			labels:     policy.Spec.PodSelector.MatchLabels,
			policyType: "ingress",
		}

		// check if there is explicitly specified PolicyTypes in the spec
		if len(policy.Spec.PolicyTypes) > 0 {
			ingressType, egressType := false, false
			for _, policyType := range policy.Spec.PolicyTypes {
				if policyType == networking.PolicyTypeIngress {
					ingressType = true
				}
				if policyType == networking.PolicyTypeEgress {
					egressType = true
				}
			}
			if ingressType && egressType {
				newPolicy.policyType = "both"
			} else if egressType {
				newPolicy.policyType = "egress"
			} else if ingressType {
				newPolicy.policyType = "ingress"
			}
		} else {
			if policy.Spec.Egress != nil && policy.Spec.Ingress != nil {
				newPolicy.policyType = "both"
			} else if policy.Spec.Egress != nil {
				newPolicy.policyType = "egress"
			} else if policy.Spec.Ingress != nil {
				newPolicy.policyType = "ingress"
			}
		}

		matchingPods, err := watchers.PodWatcher.ListByNamespaceAndLabels(policy.Namespace, policy.Spec.PodSelector.MatchLabels)
		newPolicy.targetPods = make(map[string]podInfo)
		if err == nil {
			for _, matchingPod := range matchingPods {
				newPolicy.targetPods[matchingPod.Status.PodIP] = podInfo{ip: matchingPod.Status.PodIP,
					name:      matchingPod.ObjectMeta.Name,
					namespace: matchingPod.ObjectMeta.Namespace,
					labels:    matchingPod.ObjectMeta.Labels}
			}
		}

		if policy.Spec.Ingress == nil {
			newPolicy.ingressRules = nil
		} else {
			newPolicy.ingressRules = make([]ingressRule, 0)
		}

		if policy.Spec.Egress == nil {
			newPolicy.egressRules = nil
		} else {
			newPolicy.egressRules = make([]egressRule, 0)
		}

		for _, specIngressRule := range policy.Spec.Ingress {
			ingressRule := ingressRule{}

			ingressRule.ports = make([]protocolAndPort, 0)

			// If this field is empty or missing in the spec, this rule matches all ports
			if len(specIngressRule.Ports) == 0 {
				ingressRule.matchAllPorts = true
			} else {
				ingressRule.matchAllPorts = false
				for _, port := range specIngressRule.Ports {
					protocolAndPort := protocolAndPort{protocol: string(*port.Protocol), port: port.Port.String()}
					ingressRule.ports = append(ingressRule.ports, protocolAndPort)
				}
			}

			ingressRule.srcPods = make([]podInfo, 0)

			// If this field is empty or missing in the spec, this rule matches all sources
			if len(specIngressRule.From) == 0 {
				ingressRule.matchAllSource = true
			} else {
				ingressRule.matchAllSource = false
				var matchingPods []*api.Pod
				var err error
				for _, peer := range specIngressRule.From {
					// spec must have either of PodSelector or NamespaceSelector
					if peer.PodSelector != nil {
						matchingPods, err = watchers.PodWatcher.ListByNamespaceAndLabels(policy.Namespace,
							peer.PodSelector.MatchLabels)
					} else if peer.NamespaceSelector != nil {
						namespaces, err := watchers.NamespaceWatcher.ListByLabels(peer.NamespaceSelector.MatchLabels)
						if err != nil {
							return nil, errors.New("Failed to build network policies info due to " + err.Error())
						}
						for _, namespace := range namespaces {
							namespacePods, err := watchers.PodWatcher.ListByNamespaceAndLabels(namespace.Name, nil)
							if err != nil {
								return nil, errors.New("Failed to build network policies info due to " + err.Error())
							}
							matchingPods = append(matchingPods, namespacePods...)
						}
					}
					if err == nil {
						for _, matchingPod := range matchingPods {
							ingressRule.srcPods = append(ingressRule.srcPods,
								podInfo{ip: matchingPod.Status.PodIP,
									name:      matchingPod.ObjectMeta.Name,
									namespace: matchingPod.ObjectMeta.Namespace,
									labels:    matchingPod.ObjectMeta.Labels})
						}
					}
				}
			}

			newPolicy.ingressRules = append(newPolicy.ingressRules, ingressRule)
		}

		for _, specEgressRule := range policy.Spec.Egress {
			egressRule := egressRule{}

			egressRule.ports = make([]protocolAndPort, 0)

			// If this field is empty or missing in the spec, this rule matches all ports
			if len(specEgressRule.Ports) == 0 {
				egressRule.matchAllPorts = true
			} else {
				egressRule.matchAllPorts = false
				for _, port := range specEgressRule.Ports {
					protocolAndPort := protocolAndPort{protocol: string(*port.Protocol), port: port.Port.String()}
					egressRule.ports = append(egressRule.ports, protocolAndPort)
				}
			}

			egressRule.dstPods = make([]podInfo, 0)

			// If this field is empty or missing in the spec, this rule matches all sources
			if len(specEgressRule.To) == 0 {
				egressRule.matchAllDestinations = true
			} else {
				egressRule.matchAllDestinations = false
				var matchingPods []*api.Pod
				var err error
				for _, peer := range specEgressRule.To {
					// spec must have either of PodSelector or NamespaceSelector
					if peer.PodSelector != nil {
						matchingPods, err = watchers.PodWatcher.ListByNamespaceAndLabels(policy.Namespace,
							peer.PodSelector.MatchLabels)
					} else if peer.NamespaceSelector != nil {
						namespaces, err := watchers.NamespaceWatcher.ListByLabels(peer.NamespaceSelector.MatchLabels)
						if err != nil {
							return nil, errors.New("Failed to build network policies info due to " + err.Error())
						}
						for _, namespace := range namespaces {
							namespacePods, err := watchers.PodWatcher.ListByNamespaceAndLabels(namespace.Name, nil)
							if err != nil {
								return nil, errors.New("Failed to build network policies info due to " + err.Error())
							}
							matchingPods = append(matchingPods, namespacePods...)
						}
					}
					if err == nil {
						for _, matchingPod := range matchingPods {
							egressRule.dstPods = append(egressRule.dstPods,
								podInfo{ip: matchingPod.Status.PodIP,
									name:      matchingPod.ObjectMeta.Name,
									namespace: matchingPod.ObjectMeta.Namespace,
									labels:    matchingPod.ObjectMeta.Labels})
						}
					}
				}
			}

			newPolicy.egressRules = append(newPolicy.egressRules, egressRule)
		}
		NetworkPolicies = append(NetworkPolicies, newPolicy)
	}

	return &NetworkPolicies, nil
}

func buildBetaNetworkPoliciesInfo() (*[]networkPolicyInfo, error) {

	NetworkPolicies := make([]networkPolicyInfo, 0)

	for _, policyObj := range watchers.NetworkPolicyWatcher.List() {

		policy, _ := policyObj.(*apiextensions.NetworkPolicy)
		newPolicy := networkPolicyInfo{
			name:      policy.Name,
			namespace: policy.Namespace,
			labels:    policy.Spec.PodSelector.MatchLabels,
		}
		matchingPods, err := watchers.PodWatcher.ListByNamespaceAndLabels(policy.Namespace, policy.Spec.PodSelector.MatchLabels)
		newPolicy.targetPods = make(map[string]podInfo)
		newPolicy.ingressRules = make([]ingressRule, 0)
		if err == nil {
			for _, matchingPod := range matchingPods {
				newPolicy.targetPods[matchingPod.Status.PodIP] = podInfo{ip: matchingPod.Status.PodIP,
					name:      matchingPod.ObjectMeta.Name,
					namespace: matchingPod.ObjectMeta.Namespace,
					labels:    matchingPod.ObjectMeta.Labels}
			}
		}

		for _, specIngressRule := range policy.Spec.Ingress {
			ingressRule := ingressRule{}

			ingressRule.ports = make([]protocolAndPort, 0)
			for _, port := range specIngressRule.Ports {
				protocolAndPort := protocolAndPort{protocol: string(*port.Protocol), port: port.Port.String()}
				ingressRule.ports = append(ingressRule.ports, protocolAndPort)
			}

			ingressRule.srcPods = make([]podInfo, 0)
			for _, peer := range specIngressRule.From {
				matchingPods, err := watchers.PodWatcher.ListByNamespaceAndLabels(policy.Namespace, peer.PodSelector.MatchLabels)
				if err == nil {
					for _, matchingPod := range matchingPods {
						ingressRule.srcPods = append(ingressRule.srcPods,
							podInfo{ip: matchingPod.Status.PodIP,
								name:      matchingPod.ObjectMeta.Name,
								namespace: matchingPod.ObjectMeta.Namespace,
								labels:    matchingPod.ObjectMeta.Labels})
					}
				}
			}
			newPolicy.ingressRules = append(newPolicy.ingressRules, ingressRule)
		}
		NetworkPolicies = append(NetworkPolicies, newPolicy)
	}

	return &NetworkPolicies, nil
}

func getNameSpaceDefaultPolicy(namespace string) (string, error) {
	for _, nspw := range watchers.NamespaceWatcher.List() {
		if strings.Compare(namespace, nspw.Name) == 0 {
			networkPolicyAnnotation, ok := nspw.ObjectMeta.Annotations["net.beta.kubernetes.io/network-policy"]
			var annot map[string]map[string]string
			if ok {
				err := json.Unmarshal([]byte(networkPolicyAnnotation), &annot)
				if err == nil {
					return annot["ingress"]["isolation"], nil
				}
				glog.Errorf("Skipping invalid network-policy for namespace \"%s\": %s", namespace, err)
				return "DefaultAllow", errors.New("Invalid NetworkPolicy.")
			}
			return "DefaultAllow", nil
		}
	}
	return "", errors.New("Failed to get the default ingress policy for the namespace: " + namespace)
}

func podFirewallChainName(namespace, podName string) string {
	hash := sha256.Sum256([]byte(namespace + podName))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "KUBE-POD-FW-" + encoded[:16]
}

func networkPolicyChainName(namespace, policyName string) string {
	hash := sha256.Sum256([]byte(namespace + policyName))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "KUBE-NWPLCY-" + encoded[:16]
}

func policySourcePodIpSetName(namespace, policyName string) string {
	hash := sha256.Sum256([]byte(namespace + policyName))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "KUBE-SRC-" + encoded[:16]
}

func policyDestinationPodIpSetName(namespace, policyName string) string {
	hash := sha256.Sum256([]byte(namespace + policyName))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "KUBE-DST-" + encoded[:16]
}

func policyIndexedSourcePodIpSetName(namespace, policyName string, ingressRuleNo int) string {
	hash := sha256.Sum256([]byte(namespace + policyName + "ingressrule" + strconv.Itoa(ingressRuleNo)))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "KUBE-SRC-" + encoded[:16]
}

func policyIndexedDestinationPodIpSetName(namespace, policyName string, egressRuleNo int) string {
	hash := sha256.Sum256([]byte(namespace + policyName + "egressrule" + strconv.Itoa(egressRuleNo)))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "KUBE-DST-" + encoded[:16]
}

// Cleanup cleanup configurations done
func (npc *NetworkPolicyController) Cleanup() {

	glog.Infof("Cleaning up iptables configuration permanently done by kube-router")

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Errorf("Failed to initialize iptables executor: %s", err.Error())
	}

	// delete jump rules in FORWARD chain to pod specific firewall chain
	forwardChainRules, err := iptablesCmdHandler.List("filter", "FORWARD")
	if err != nil {
		glog.Errorf("Failed to delete iptable rules as part of cleanup")
		return
	}

	// TODO: need a better way to delte rule with out using number
	var realRuleNo int
	for i, rule := range forwardChainRules {
		if strings.Contains(rule, "KUBE-POD-FW-") {
			err = iptablesCmdHandler.Delete("filter", "FORWARD", strconv.Itoa(i-realRuleNo))
			realRuleNo++
		}
	}

	// delete jump rules in OUTPUT chain to pod specific firewall chain
	forwardChainRules, err = iptablesCmdHandler.List("filter", "OUTPUT")
	if err != nil {
		glog.Errorf("Failed to delete iptable rules as part of cleanup")
		return
	}

	// TODO: need a better way to delte rule with out using number
	realRuleNo = 0
	for i, rule := range forwardChainRules {
		if strings.Contains(rule, "KUBE-POD-FW-") {
			err = iptablesCmdHandler.Delete("filter", "OUTPUT", strconv.Itoa(i-realRuleNo))
			realRuleNo++
		}
	}

	// flush and delete pod specific firewall chain
	chains, err := iptablesCmdHandler.ListChains("filter")
	for _, chain := range chains {
		if strings.HasPrefix(chain, "KUBE-POD-FW-") {
			err = iptablesCmdHandler.ClearChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptable rules: " + err.Error())
				return
			}
			err = iptablesCmdHandler.DeleteChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptable rules: " + err.Error())
				return
			}
		}
	}

	// flush and delete per network policy specific chain
	chains, err = iptablesCmdHandler.ListChains("filter")
	for _, chain := range chains {
		if strings.HasPrefix(chain, "KUBE-NWPLCY-") {
			err = iptablesCmdHandler.ClearChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptable rules: " + err.Error())
				return
			}
			err = iptablesCmdHandler.DeleteChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptable rules: " + err.Error())
				return
			}
		}
	}

	// delete all ipsets
	err = npc.ipSetHandler.DestroyAllWithin()
	if err != nil {
		glog.Errorf("Failed to clean up ipsets: " + err.Error())
	}
	glog.Infof("Successfully cleaned the iptables configuration done by kube-router")
}

// NewNetworkPolicyController returns new NetworkPolicyController object
func NewNetworkPolicyController(clientset *kubernetes.Clientset, config *options.KubeRouterConfig) (*NetworkPolicyController, error) {

	npc := NetworkPolicyController{}

	npc.syncPeriod = config.IPTablesSyncPeriod

	npc.v1NetworkPolicy = true
	v, _ := clientset.Discovery().ServerVersion()
	minorVer, _ := strconv.Atoi(v.Minor)
	if v.Major == "1" && minorVer < 7 {
		npc.v1NetworkPolicy = false
	}

	node, err := utils.GetNodeObject(clientset, config.HostnameOverride)
	if err != nil {
		return nil, err
	}

	npc.nodeHostName = node.Name

	nodeIP, err := utils.GetNodeIP(node)
	if err != nil {
		return nil, err
	}
	npc.nodeIP = nodeIP

	ipset, err := utils.NewIPSet()
	if err != nil {
		return nil, err
	}
	err = ipset.Save()
	if err != nil {
		return nil, err
	}
	npc.ipSetHandler = ipset

	watchers.PodWatcher.RegisterHandler(&npc)
	watchers.NetworkPolicyWatcher.RegisterHandler(&npc)
	watchers.NamespaceWatcher.RegisterHandler(&npc)

	return &npc, nil
}
