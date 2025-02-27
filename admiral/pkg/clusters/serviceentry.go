package clusters

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/istio-ecosystem/admiral/admiral/pkg/apis/admiral/v1"

	argo "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/client-go/pkg/apis/networking/v1alpha3"
	k8sAppsV1 "k8s.io/api/apps/v1"
	k8sV1 "k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/istio-ecosystem/admiral/admiral/pkg/controller/admiral"
	"github.com/istio-ecosystem/admiral/admiral/pkg/controller/common"
	"github.com/istio-ecosystem/admiral/admiral/pkg/controller/util"
)

type SeDrTuple struct {
	SeName          string
	DrName          string
	ServiceEntry    *networking.ServiceEntry
	DestinationRule *networking.DestinationRule
}

func createServiceEntry(event admiral.EventType, rc *RemoteController, admiralCache *AdmiralCache,
	meshPorts map[string]uint32, destDeployment *k8sAppsV1.Deployment, serviceEntries map[string]*networking.ServiceEntry) *networking.ServiceEntry {

	workloadIdentityKey := common.GetWorkloadIdentifier()
	globalFqdn := common.GetCname(destDeployment, workloadIdentityKey, common.GetHostnameSuffix())

	//Handling retries for getting/putting service entries from/in cache

	address := getUniqueAddress(admiralCache, globalFqdn)

	if len(globalFqdn) == 0 || len(address) == 0 {
		return nil
	}

	san := getSanForDeployment(destDeployment, workloadIdentityKey)

	tmpSe := generateServiceEntry(event, admiralCache, meshPorts, globalFqdn, rc, serviceEntries, address, san)
	return tmpSe
}

func modifyServiceEntryForNewServiceOrPod(event admiral.EventType, env string, sourceIdentity string, remoteRegistry *RemoteRegistry) map[string]*networking.ServiceEntry {

	defer util.LogElapsedTime("modifyServiceEntryForNewServiceOrPod", sourceIdentity, env, "")()

	if CurrentAdmiralState.ReadOnly {
		log.Infof(LogFormat, event, env, sourceIdentity, "", "Processing skipped as Admiral is in Read-only mode")
		return nil
	}

	if IsCacheWarmupTime(remoteRegistry) {
		log.Infof(LogFormat, event, env, sourceIdentity, "", "Processing skipped during cache warm up state")
		return nil
	}
	//create a service entry, destination rule and virtual service in the local cluster
	sourceServices := make(map[string]*k8sV1.Service)
	sourceWeightedServices := make(map[string]map[string]*WeightedService)
	sourceDeployments := make(map[string]*k8sAppsV1.Deployment)
	sourceRollouts := make(map[string]*argo.Rollout)

	var serviceEntries = make(map[string]*networking.ServiceEntry)

	var cname string
	cnames := make(map[string]string)
	var serviceInstance *k8sV1.Service
	var weightedServices map[string]*WeightedService
	var rollout *argo.Rollout
	var deployment *k8sAppsV1.Deployment
	var gtps = make(map[string][]*v1.GlobalTrafficPolicy)

	var namespace string

	var gtpKey = common.ConstructGtpKey(env, sourceIdentity)

	start := time.Now()

	clusters := remoteRegistry.GetClusterIds()

	for _, clusterId := range clusters {

		rc := remoteRegistry.GetRemoteController(clusterId)

		if rc == nil {
			log.Warnf(LogFormat, "Find", "remote-controller", clusterId, clusterId, "remote controller not available/initialized for the cluster")
			continue
		}

		if rc.DeploymentController != nil {
			deployment = rc.DeploymentController.Cache.Get(sourceIdentity, env)
		}

		if rc.RolloutController != nil {
			rollout = rc.RolloutController.Cache.Get(sourceIdentity, env)
		}

		if deployment != nil {
			remoteRegistry.AdmiralCache.IdentityClusterCache.Put(sourceIdentity, rc.ClusterID, rc.ClusterID)
			serviceInstance = getServiceForDeployment(rc, deployment)
			if serviceInstance == nil {
				continue
			}
			namespace = deployment.Namespace
			localMeshPorts := GetMeshPorts(rc.ClusterID, serviceInstance, deployment)

			cname = common.GetCname(deployment, common.GetWorkloadIdentifier(), common.GetHostnameSuffix())
			sourceDeployments[rc.ClusterID] = deployment
			createServiceEntry(event, rc, remoteRegistry.AdmiralCache, localMeshPorts, deployment, serviceEntries)
		} else if rollout != nil {
			remoteRegistry.AdmiralCache.IdentityClusterCache.Put(sourceIdentity, rc.ClusterID, rc.ClusterID)
			weightedServices = getServiceForRollout(rc, rollout)
			if len(weightedServices) == 0 {
				continue
			}

			//use any service within the weightedServices for determining ports etc.
			for _, sInstance := range weightedServices {
				serviceInstance = sInstance.Service
				break
			}
			namespace = rollout.Namespace
			localMeshPorts := GetMeshPortsForRollout(rc.ClusterID, serviceInstance, rollout)

			cname = common.GetCnameForRollout(rollout, common.GetWorkloadIdentifier(), common.GetHostnameSuffix())
			cnames[cname] = "1"
			sourceRollouts[rc.ClusterID] = rollout
			createServiceEntryForRollout(event, rc, remoteRegistry.AdmiralCache, localMeshPorts, rollout, serviceEntries)
		} else {
			continue
		}

		gtpsInNamespace := rc.GlobalTraffic.Cache.Get(gtpKey, namespace)
		if len(gtpsInNamespace) > 0 {
			if log.IsLevelEnabled(log.DebugLevel) {
				log.Debugf("GTPs found for identity=%s in env=%s namespace=%s gtp=%v", sourceIdentity, env, namespace, gtpsInNamespace)
			}
			gtps[rc.ClusterID] = gtpsInNamespace
		} else {
			log.Debugf("No GTPs found for identity=%s in env=%s namespace=%s with key=%s", sourceIdentity, env, namespace, gtpKey)
		}

		remoteRegistry.AdmiralCache.CnameClusterCache.Put(cname, rc.ClusterID, rc.ClusterID)
		remoteRegistry.AdmiralCache.CnameIdentityCache.Store(cname, sourceIdentity)
		sourceServices[rc.ClusterID] = serviceInstance
		sourceWeightedServices[rc.ClusterID] = weightedServices
	}

	util.LogElapsedTimeSince("BuildServiceEntry", sourceIdentity, env, "", start)

	//cache the latest GTP in global cache to be reused during DR creation
	updateGlobalGtpCache(remoteRegistry.AdmiralCache, sourceIdentity, env, gtps)

	dependents := remoteRegistry.AdmiralCache.IdentityDependencyCache.Get(sourceIdentity).Copy()

	//handle local updates (source clusters first)
	//update the address to local fqdn for service entry in a cluster local to the service instance

	start = time.Now()

	for sourceCluster, serviceInstance := range sourceServices {
		localFqdn := serviceInstance.Name + common.Sep + serviceInstance.Namespace + common.DotLocalDomainSuffix
		rc := remoteRegistry.GetRemoteController(sourceCluster)
		var meshPorts map[string]uint32
		blueGreenStrategy := isBlueGreenStrategy(sourceRollouts[sourceCluster])

		if len(sourceDeployments) > 0 {
			meshPorts = GetMeshPorts(sourceCluster, serviceInstance, sourceDeployments[sourceCluster])
		} else {
			meshPorts = GetMeshPortsForRollout(sourceCluster, serviceInstance, sourceRollouts[sourceCluster])
		}

		for key, serviceEntry := range serviceEntries {
			if len(serviceEntry.Endpoints) == 0 {
				AddServiceEntriesWithDr(remoteRegistry, map[string]string{sourceCluster: sourceCluster},
					map[string]*networking.ServiceEntry{key: serviceEntry})
			}
			clusterIngress, _ := rc.ServiceController.Cache.GetLoadBalancer(common.GetAdmiralParams().LabelSet.GatewayApp, common.NamespaceIstioSystem)
			for _, ep := range serviceEntry.Endpoints {
				//replace istio ingress-gateway address with local fqdn, note that ingress-gateway can be empty (not provisoned, or is not up)
				if ep.Address == clusterIngress || ep.Address == "" {
					// Update endpoints with locafqdn for active and preview se of bluegreen rollout
					if blueGreenStrategy {
						oldPorts := ep.Ports
						updateEndpointsForBlueGreen(sourceRollouts[sourceCluster], sourceWeightedServices[sourceCluster], cnames, ep, sourceCluster, key)

						AddServiceEntriesWithDr(remoteRegistry, map[string]string{sourceCluster: sourceCluster},
							map[string]*networking.ServiceEntry{key: serviceEntry})
						//swap it back to use for next iteration
						ep.Address = clusterIngress
						ep.Ports = oldPorts
						// see if we have weighted services (rollouts with canary strategy)
					} else if len(sourceWeightedServices[sourceCluster]) > 1 {
						//add one endpoint per each service, may be modify
						var se = copyServiceEntry(serviceEntry)
						updateEndpointsForWeightedServices(se, sourceWeightedServices[sourceCluster], clusterIngress, meshPorts)
						AddServiceEntriesWithDr(remoteRegistry, map[string]string{sourceCluster: sourceCluster},
							map[string]*networking.ServiceEntry{key: se})
					} else {
						ep.Address = localFqdn
						oldPorts := ep.Ports
						ep.Ports = meshPorts
						AddServiceEntriesWithDr(remoteRegistry, map[string]string{sourceCluster: sourceCluster},
							map[string]*networking.ServiceEntry{key: serviceEntry})
						//swap it back to use for next iteration
						ep.Address = clusterIngress
						ep.Ports = oldPorts
					}
				}
			}
		}

		if common.GetWorkloadSidecarUpdate() == "enabled" {
			modifySidecarForLocalClusterCommunication(serviceInstance.Namespace, remoteRegistry.AdmiralCache.DependencyNamespaceCache.Get(sourceIdentity), rc)
		}

		for _, val := range dependents {
			remoteRegistry.AdmiralCache.DependencyNamespaceCache.Put(val, serviceInstance.Namespace, localFqdn, cnames)
		}

	}

	util.LogElapsedTimeSince("WriteServiceEntryToSourceClusters", sourceIdentity, env, "", start)

	//Write to dependent clusters

	start = time.Now()

	dependentClusters := getDependentClusters(dependents, remoteRegistry.AdmiralCache.IdentityClusterCache, sourceServices)

	//update cname dependent cluster cache
	for clusterId := range dependentClusters {
		remoteRegistry.AdmiralCache.CnameDependentClusterCache.Put(cname, clusterId, clusterId)
	}

	AddServiceEntriesWithDr(remoteRegistry, dependentClusters, serviceEntries)

	util.LogElapsedTimeSince("WriteServiceEntryToDependentClusters", sourceIdentity, env, "", start)

	return serviceEntries
}

//Does two things;
//i)  Picks the GTP that was created most recently from the passed in GTP list based on GTP priority label (GTPs from all clusters)
//ii) Updates the global GTP cache with the selected GTP in i)
func updateGlobalGtpCache(cache *AdmiralCache, identity, env string, gtps map[string][]*v1.GlobalTrafficPolicy) {
	defer util.LogElapsedTime("updateGlobalGtpCache", identity, env, "")()
	gtpsOrdered := make([]*v1.GlobalTrafficPolicy, 0)
	for _, gtpsInCluster := range gtps {
		gtpsOrdered = append(gtpsOrdered, gtpsInCluster...)
	}
	if len(gtpsOrdered) == 0 {
		log.Debugf("No GTPs found for identity=%s in env=%s. Deleting global cache entries if any", identity, env)
		cache.GlobalTrafficCache.Delete(identity, env)
		return
	} else if len(gtpsOrdered) > 1 {
		log.Debugf("More than one GTP found for identity=%s in env=%s.", identity, env)
		//sort by creation time and priority, gtp with highest priority and most recent at the beginning
		sortGtpsByPriorityAndCreationTime(gtpsOrdered, identity, env)
	}

	mostRecentGtp := gtpsOrdered[0]

	err := cache.GlobalTrafficCache.Put(mostRecentGtp)

	if err != nil {
		log.Errorf("Error in updating GTP with name=%s in namespace=%s as actively used for identity=%s with err=%v", mostRecentGtp.Name, mostRecentGtp.Namespace, common.GetGtpKey(mostRecentGtp), err)
	} else {
		log.Infof("GTP with name=%s in namespace=%s is actively used for identity=%s", mostRecentGtp.Name, mostRecentGtp.Namespace, common.GetGtpKey(mostRecentGtp))
	}
}

func sortGtpsByPriorityAndCreationTime(gtpsToOrder []*v1.GlobalTrafficPolicy, identity string, env string) {
	sort.Slice(gtpsToOrder, func(i, j int) bool {
		iPriority := getGtpPriority(gtpsToOrder[i])
		jPriority := getGtpPriority(gtpsToOrder[j])

		iTime := gtpsToOrder[i].CreationTimestamp
		jTime := gtpsToOrder[j].CreationTimestamp

		if iPriority != jPriority {
			log.Debugf("GTP sorting identity=%s env=%s name1=%s creationTime1=%v priority1=%d name2=%s creationTime2=%v priority2=%d", identity, env, gtpsToOrder[i].Name, iTime, iPriority, gtpsToOrder[j].Name, jTime, jPriority)
			return iPriority > jPriority
		}
		log.Debugf("GTP sorting identity=%s env=%s name1=%s creationTime1=%v priority1=%d name2=%s creationTime2=%v priority2=%d", identity, env, gtpsToOrder[i].Name, iTime, iPriority, gtpsToOrder[j].Name, jTime, jPriority)
		return iTime.After(jTime.Time)
	})
}
func getGtpPriority(gtp *v1.GlobalTrafficPolicy) int {
	if val, ok := gtp.ObjectMeta.Labels[common.GetAdmiralParams().LabelSet.PriorityKey]; ok {
		if convertedValue, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			return convertedValue
		}
	}
	return 0
}
func updateEndpointsForBlueGreen(rollout *argo.Rollout, weightedServices map[string]*WeightedService, cnames map[string]string,
	ep *networking.ServiceEntry_Endpoint, sourceCluster string, meshHost string) {
	activeServiceName := rollout.Spec.Strategy.BlueGreen.ActiveService
	previewServiceName := rollout.Spec.Strategy.BlueGreen.PreviewService

	if previewService, ok := weightedServices[previewServiceName]; strings.HasPrefix(meshHost, common.BlueGreenRolloutPreviewPrefix+common.Sep) && ok {
		previewServiceInstance := previewService.Service
		localFqdn := previewServiceInstance.Name + common.Sep + previewServiceInstance.Namespace + common.DotLocalDomainSuffix
		cnames[localFqdn] = "1"
		ep.Address = localFqdn
		ep.Ports = GetMeshPortsForRollout(sourceCluster, previewServiceInstance, rollout)
	} else if activeService, ok := weightedServices[activeServiceName]; ok {
		activeServiceInstance := activeService.Service
		localFqdn := activeServiceInstance.Name + common.Sep + activeServiceInstance.Namespace + common.DotLocalDomainSuffix
		cnames[localFqdn] = "1"
		ep.Address = localFqdn
		ep.Ports = GetMeshPortsForRollout(sourceCluster, activeServiceInstance, rollout)
	}
}

//update endpoints for Argo rollouts specific Service Entries to account for traffic splitting (Canary strategy)
func updateEndpointsForWeightedServices(serviceEntry *networking.ServiceEntry, weightedServices map[string]*WeightedService, clusterIngress string, meshPorts map[string]uint32) {
	var endpoints = make([]*networking.ServiceEntry_Endpoint, 0)
	var endpointToReplace *networking.ServiceEntry_Endpoint

	//collect all endpoints except the one to replace
	for _, ep := range serviceEntry.Endpoints {
		if ep.Address == clusterIngress || ep.Address == "" {
			endpointToReplace = ep
		} else {
			endpoints = append(endpoints, ep)
		}
	}

	if endpointToReplace == nil {
		return
	}

	//create endpoints based on weightedServices
	for _, serviceInstance := range weightedServices {
		//skip service instances with 0 weight
		if serviceInstance.Weight <= 0 {
			continue
		}
		var ep = copyEndpoint(endpointToReplace)
		ep.Ports = meshPorts
		ep.Address = serviceInstance.Service.Name + common.Sep + serviceInstance.Service.Namespace + common.DotLocalDomainSuffix
		ep.Weight = uint32(serviceInstance.Weight)
		endpoints = append(endpoints, ep)
	}
	serviceEntry.Endpoints = endpoints
}

func modifySidecarForLocalClusterCommunication(sidecarNamespace string, sidecarEgressMap map[string]common.SidecarEgress, rc *RemoteController) {

	//get existing sidecar from the cluster
	sidecarConfig := rc.SidecarController

	if sidecarConfig == nil || sidecarEgressMap == nil {
		return
	}

	sidecar, _ := sidecarConfig.IstioClient.NetworkingV1alpha3().Sidecars(sidecarNamespace).Get(common.GetWorkloadSidecarName(), v12.GetOptions{})

	if sidecar == nil || (sidecar.Spec.Egress == nil) {
		return
	}

	//copy and add our new local FQDN
	newSidecar := copySidecar(sidecar)

	egressHosts := make(map[string]string)

	for _, sidecarEgress := range sidecarEgressMap {
		egressHost := sidecarEgress.Namespace + "/" + sidecarEgress.FQDN
		egressHosts[egressHost] = egressHost
		for cname := range sidecarEgress.CNAMEs {
			scopedCname := sidecarEgress.Namespace + "/" + cname
			egressHosts[scopedCname] = scopedCname
		}
	}

	for egressHost := range egressHosts {
		if !util.Contains(newSidecar.Spec.Egress[0].Hosts, egressHost) {
			newSidecar.Spec.Egress[0].Hosts = append(newSidecar.Spec.Egress[0].Hosts, egressHost)
		}
	}

	newSidecarConfig := createSidecarSkeletion(newSidecar.Spec, common.GetWorkloadSidecarName(), sidecarNamespace)

	//insert into cluster
	if newSidecarConfig != nil {
		addUpdateSidecar(newSidecarConfig, sidecar, sidecarNamespace, rc)
	}
}

func addUpdateSidecar(obj *v1alpha3.Sidecar, exist *v1alpha3.Sidecar, namespace string, rc *RemoteController) {
	var err error
	exist.Labels = obj.Labels
	exist.Annotations = obj.Annotations
	exist.Spec = obj.Spec
	_, err = rc.SidecarController.IstioClient.NetworkingV1alpha3().Sidecars(namespace).Update(exist)

	if err != nil {
		log.Infof(LogErrFormat, "Update", "Sidecar", obj.Name, rc.ClusterID, err)
	} else {
		log.Infof(LogErrFormat, "Update", "Sidecar", obj.Name, rc.ClusterID, "Success")
	}
}

func copySidecar(sidecar *v1alpha3.Sidecar) *v1alpha3.Sidecar {
	newSidecarObj := &v1alpha3.Sidecar{}
	newSidecarObj.Spec.WorkloadSelector = sidecar.Spec.WorkloadSelector
	newSidecarObj.Spec.Ingress = sidecar.Spec.Ingress
	newSidecarObj.Spec.Egress = sidecar.Spec.Egress
	return newSidecarObj
}

//This will create the default service entries and also additional ones specified in GTP
func AddServiceEntriesWithDr(rr *RemoteRegistry, sourceClusters map[string]string, serviceEntries map[string]*networking.ServiceEntry) {

	cache := rr.AdmiralCache

	syncNamespace := common.GetSyncNamespace()
	for _, se := range serviceEntries {

		var identityId string
		if identityValue, ok := cache.CnameIdentityCache.Load(se.Hosts[0]); ok {
			identityId = fmt.Sprint(identityValue)
		}

		splitByEnv := strings.Split(se.Hosts[0], common.Sep)
		var env = splitByEnv[0]

		globalTrafficPolicy := cache.GlobalTrafficCache.GetFromIdentity(identityId, env)

		for _, sourceCluster := range sourceClusters {

			rc := rr.GetRemoteController(sourceCluster)

			if rc == nil || rc.NodeController == nil || rc.NodeController.Locality == nil {
				log.Warnf(LogFormat, "Find", "remote-controller", sourceCluster, sourceCluster, "locality not available for the cluster")
				continue
			}

			//check if there is a gtp and add additional hosts/destination rules
			var seDrSet = createSeAndDrSetFromGtp(env, rc.NodeController.Locality.Region, se, globalTrafficPolicy, cache)

			for _, seDr := range seDrSet {
				oldServiceEntry, err := rc.ServiceEntryController.IstioClient.NetworkingV1alpha3().ServiceEntries(syncNamespace).Get(seDr.SeName, v12.GetOptions{})
				// if old service entry not find, just create a new service entry instead
				if err != nil {
					log.Infof(LogFormat, "Get (error)", "old ServiceEntry", seDr.SeName, sourceCluster, err)
					oldServiceEntry = nil
				}
				oldDestinationRule, err := rc.DestinationRuleController.IstioClient.NetworkingV1alpha3().DestinationRules(syncNamespace).Get(seDr.DrName, v12.GetOptions{})

				if err != nil {
					log.Infof(LogFormat, "Get (error)", "old DestinationRule", seDr.DrName, sourceCluster, err)
					oldDestinationRule = nil
				}

				if len(seDr.ServiceEntry.Endpoints) == 0 {
					deleteServiceEntry(oldServiceEntry, syncNamespace, rc)
					cache.SeClusterCache.Delete(seDr.ServiceEntry.Hosts[0])
					// after deleting the service entry, destination rule also need to be deleted if the service entry host no longer exists
					deleteDestinationRule(oldDestinationRule, syncNamespace, rc)
				} else {
					newServiceEntry := createServiceEntrySkeletion(*seDr.ServiceEntry, seDr.SeName, syncNamespace)

					if newServiceEntry != nil {
						newServiceEntry.Labels = map[string]string{common.GetWorkloadIdentifier(): fmt.Sprintf("%v", identityId)}
						addUpdateServiceEntry(newServiceEntry, oldServiceEntry, syncNamespace, rc)
						cache.SeClusterCache.Put(newServiceEntry.Spec.Hosts[0], rc.ClusterID, rc.ClusterID)
					}

					newDestinationRule := createDestinationRuleSkeletion(*seDr.DestinationRule, seDr.DrName, syncNamespace)
					// if event was deletion when this function was called, then GlobalTrafficCache should already deleted the cache globalTrafficPolicy is an empty shell object
					addUpdateDestinationRule(newDestinationRule, oldDestinationRule, syncNamespace, rc)
				}
			}
		}
	}
}

func createSeAndDrSetFromGtp(env, region string, se *networking.ServiceEntry, globalTrafficPolicy *v1.GlobalTrafficPolicy,
	cache *AdmiralCache) map[string]*SeDrTuple {
	var defaultDrName = getIstioResourceName(se.Hosts[0], "-default-dr")
	var defaultSeName = getIstioResourceName(se.Hosts[0], "-se")
	var seDrSet = make(map[string]*SeDrTuple)
	if globalTrafficPolicy != nil {
		gtp := globalTrafficPolicy.Spec
		for _, gtpTrafficPolicy := range gtp.Policy {
			var modifiedSe = se
			var host = se.Hosts[0]
			var drName, seName = defaultDrName, defaultSeName
			if gtpTrafficPolicy.Dns != "" {
				log.Warnf("Using the deprecated field `dns` in gtp: %v in namespace: %v", globalTrafficPolicy.Name, globalTrafficPolicy.Namespace)
			}
			if gtpTrafficPolicy.DnsPrefix != env && gtpTrafficPolicy.DnsPrefix != common.Default &&
				gtpTrafficPolicy.Dns != host {
				host = common.GetCnameVal([]string{gtpTrafficPolicy.DnsPrefix, se.Hosts[0]})
				drName, seName = getIstioResourceName(host, "-dr"), getIstioResourceName(host, "-se")
				modifiedSe = copyServiceEntry(se)
				modifiedSe.Hosts[0] = host
				modifiedSe.Addresses[0] = getUniqueAddress(cache, host)
			}
			var seDr = &SeDrTuple{
				DrName:          drName,
				SeName:          seName,
				DestinationRule: getDestinationRule(modifiedSe, region, gtpTrafficPolicy),
				ServiceEntry:    modifiedSe,
			}
			seDrSet[host] = seDr
		}
	}
	//create a destination rule for default hostname if that wasn't overriden in gtp
	if _, ok := seDrSet[se.Hosts[0]]; !ok {
		var seDr = &SeDrTuple{
			DrName:          defaultDrName,
			SeName:          defaultSeName,
			DestinationRule: getDestinationRule(se, region, nil),
			ServiceEntry:    se,
		}
		seDrSet[se.Hosts[0]] = seDr
	}
	return seDrSet
}

func makeRemoteEndpointForServiceEntry(address string, locality string, portName string, portNumber int) *networking.ServiceEntry_Endpoint {
	return &networking.ServiceEntry_Endpoint{Address: address,
		Locality: locality,
		Ports:    map[string]uint32{portName: uint32(portNumber)}} //
}

func copyServiceEntry(se *networking.ServiceEntry) *networking.ServiceEntry {
	var newSe = &networking.ServiceEntry{}
	se.DeepCopyInto(newSe)
	return newSe
}

func loadServiceEntryCacheData(c admiral.ConfigMapControllerInterface, admiralCache *AdmiralCache) {
	configmap, err := c.GetConfigMap()
	if err != nil {
		log.Warnf("Failed to refresh configmap state Error: %v", err)
		return //No need to invalidate the cache
	}

	entryCache := GetServiceEntryStateFromConfigmap(configmap)

	if entryCache != nil {
		*admiralCache.ServiceEntryAddressStore = *entryCache
		log.Infof("Successfully updated service entry cache state")
	}

}

//Gets a guarenteed unique local address for a serviceentry. Returns the address, True iff the configmap was updated false otherwise, and an error if any
//Any error coupled with an empty string address means the method should be retried
func GetLocalAddressForSe(seName string, seAddressCache *ServiceEntryAddressStore, configMapController admiral.ConfigMapControllerInterface) (string, bool, error) {
	var address = seAddressCache.EntryAddresses[seName]
	if len(address) == 0 {
		address, err := GenerateNewAddressAndAddToConfigMap(seName, configMapController)
		return address, true, err
	}
	return address, false, nil
}

func GetServiceEntriesByCluster(clusterID string, remoteRegistry *RemoteRegistry) ([]v1alpha3.ServiceEntry, error) {
	remoteController := remoteRegistry.GetRemoteController(clusterID)
	if remoteController != nil {
		serviceEnteries, err := remoteController.ServiceEntryController.IstioClient.NetworkingV1alpha3().ServiceEntries(common.GetSyncNamespace()).List(v12.ListOptions{})

		if err != nil {
			log.Errorf(LogFormat, "Get", "ServiceEntries", "", clusterID, err)
			return nil, err
		}

		return serviceEnteries.Items, nil
	} else {
		err := fmt.Errorf("Admiral is not monitoring cluster %s", clusterID)
		return nil, err
	}
}

//an atomic fetch and update operation against the configmap (using K8s built in optimistic consistency mechanism via resource version)
func GenerateNewAddressAndAddToConfigMap(seName string, configMapController admiral.ConfigMapControllerInterface) (string, error) {
	//1. get cm, see if there. 2. gen new uq address. 3. put configmap. RETURN SUCCESSFULLY IFF CONFIGMAP PUT SUCCEEDS
	cm, err := configMapController.GetConfigMap()
	if err != nil {
		return "", err
	}

	newAddressState := GetServiceEntryStateFromConfigmap(cm)

	if newAddressState == nil {
		return "", errors.New("could not unmarshall configmap yaml")
	}

	if val, ok := newAddressState.EntryAddresses[seName]; ok { //Someone else updated the address state, so we'll use that
		return val, nil
	}

	secondIndex := (len(newAddressState.Addresses) / 255) + 10
	firstIndex := (len(newAddressState.Addresses) % 255) + 1
	address := configMapController.GetIPPrefixForServiceEntries() + common.Sep + strconv.Itoa(secondIndex) + common.Sep + strconv.Itoa(firstIndex)

	for util.Contains(newAddressState.Addresses, address) {
		if firstIndex < 255 {
			firstIndex++
		} else {
			secondIndex++
			firstIndex = 0
		}
		address = configMapController.GetIPPrefixForServiceEntries() + common.Sep + strconv.Itoa(secondIndex) + common.Sep + strconv.Itoa(firstIndex)
	}
	newAddressState.Addresses = append(newAddressState.Addresses, address)
	newAddressState.EntryAddresses[seName] = address

	err = putServiceEntryStateFromConfigmap(configMapController, cm, newAddressState)

	if err != nil {
		return "", err
	}
	return address, nil
}

//puts new data into an existing configmap. Providing the original is necessary to prevent fetch and update race conditions
func putServiceEntryStateFromConfigmap(c admiral.ConfigMapControllerInterface, originalConfigmap *k8sV1.ConfigMap, data *ServiceEntryAddressStore) error {
	if originalConfigmap == nil {
		return errors.New("configmap must not be nil")
	}

	bytes, err := yaml.Marshal(data)

	if err != nil {
		log.Errorf("Failed to put service entry state into the configmap. %v", err)
		return err
	}

	if originalConfigmap.Data == nil {
		originalConfigmap.Data = map[string]string{}
	}

	originalConfigmap.Data["serviceEntryAddressStore"] = string(bytes)

	err = ValidateConfigmapBeforePutting(originalConfigmap)
	if err != nil {
		log.Errorf("Configmap failed validation. Something is wrong. Error: %v", err)
		return err
	}

	return c.PutConfigMap(originalConfigmap)
}

func createServiceEntryForRollout(event admiral.EventType, rc *RemoteController, admiralCache *AdmiralCache,
	meshPorts map[string]uint32, destRollout *argo.Rollout, serviceEntries map[string]*networking.ServiceEntry) *networking.ServiceEntry {

	workloadIdentityKey := common.GetWorkloadIdentifier()
	globalFqdn := common.GetCnameForRollout(destRollout, workloadIdentityKey, common.GetHostnameSuffix())

	//Handling retries for getting/putting service entries from/in cache

	address := getUniqueAddress(admiralCache, globalFqdn)

	if len(globalFqdn) == 0 || len(address) == 0 {
		return nil
	}

	san := getSanForRollout(destRollout, workloadIdentityKey)

	if destRollout.Spec.Strategy.BlueGreen != nil && destRollout.Spec.Strategy.BlueGreen.PreviewService != "" {
		rolloutServices := getServiceForRollout(rc, destRollout)
		if _, ok := rolloutServices[destRollout.Spec.Strategy.BlueGreen.PreviewService]; ok {
			previewGlobalFqdn := common.BlueGreenRolloutPreviewPrefix + common.Sep + common.GetCnameForRollout(destRollout, workloadIdentityKey, common.GetHostnameSuffix())
			previewAddress := getUniqueAddress(admiralCache, previewGlobalFqdn)
			if len(previewGlobalFqdn) != 0 && len(previewAddress) != 0 {
				generateServiceEntry(event, admiralCache, meshPorts, previewGlobalFqdn, rc, serviceEntries, previewAddress, san)
			}
		}
	}

	tmpSe := generateServiceEntry(event, admiralCache, meshPorts, globalFqdn, rc, serviceEntries, address, san)
	return tmpSe
}

func getSanForDeployment(destDeployment *k8sAppsV1.Deployment, workloadIdentityKey string) (san []string) {
	if common.GetEnableSAN() {
		tmpSan := common.GetSAN(common.GetSANPrefix(), destDeployment, workloadIdentityKey)
		if len(tmpSan) > 0 {
			return []string{common.GetSAN(common.GetSANPrefix(), destDeployment, workloadIdentityKey)}
		}
	}
	return nil

}

func getSanForRollout(destRollout *argo.Rollout, workloadIdentityKey string) (san []string) {
	if common.GetEnableSAN() {
		tmpSan := common.GetSANForRollout(common.GetSANPrefix(), destRollout, workloadIdentityKey)
		if len(tmpSan) > 0 {
			return []string{common.GetSANForRollout(common.GetSANPrefix(), destRollout, workloadIdentityKey)}
		}
	}
	return nil

}

func getUniqueAddress(admiralCache *AdmiralCache, globalFqdn string) (address string) {

	//initializations
	var err error = nil
	maxRetries := 3
	counter := 0
	address = ""
	needsCacheUpdate := false

	for err == nil && counter < maxRetries {
		address, needsCacheUpdate, err = GetLocalAddressForSe(getIstioResourceName(globalFqdn, "-se"), admiralCache.ServiceEntryAddressStore, admiralCache.ConfigMapController)

		if err != nil {
			log.Errorf("Error getting local address for Service Entry. Err: %v", err)
			break
		}

		//random expo backoff
		timeToBackoff := rand.Intn(int(math.Pow(100.0, float64(counter)))) //get a random number between 0 and 100^counter. Will always be 0 the first time, will be 0-100 the second, and 0-1000 the third
		time.Sleep(time.Duration(timeToBackoff) * time.Millisecond)

		counter++
	}

	if err != nil {
		log.Errorf("Could not get unique address after %v retries. Failing to create serviceentry name=%v", maxRetries, globalFqdn)
		return address
	}

	if needsCacheUpdate {
		loadServiceEntryCacheData(admiralCache.ConfigMapController, admiralCache)
	}

	return address
}

func generateServiceEntry(event admiral.EventType, admiralCache *AdmiralCache, meshPorts map[string]uint32, globalFqdn string, rc *RemoteController, serviceEntries map[string]*networking.ServiceEntry, address string, san []string) *networking.ServiceEntry {
	admiralCache.CnameClusterCache.Put(globalFqdn, rc.ClusterID, rc.ClusterID)

	tmpSe := serviceEntries[globalFqdn]

	var finalProtocol = common.Http

	var sePorts = []*networking.Port{{Number: uint32(common.DefaultServiceEntryPort),
		Name: finalProtocol, Protocol: finalProtocol}}

	for protocol := range meshPorts {
		sePorts = []*networking.Port{{Number: uint32(common.DefaultServiceEntryPort),
			Name: protocol, Protocol: protocol}}
		finalProtocol = protocol
	}

	if tmpSe == nil {
		tmpSe = &networking.ServiceEntry{
			Hosts:           []string{globalFqdn},
			Ports:           sePorts,
			Location:        networking.ServiceEntry_MESH_INTERNAL,
			Resolution:      networking.ServiceEntry_DNS,
			Addresses:       []string{address}, //It is possible that the address is an empty string. That is fine as the se creation will fail and log an error
			SubjectAltNames: san,
		}
		tmpSe.Endpoints = []*networking.ServiceEntry_Endpoint{}
	}

	endpointAddress, port := rc.ServiceController.Cache.GetLoadBalancer(common.GetAdmiralParams().LabelSet.GatewayApp, common.NamespaceIstioSystem)

	var locality string
	if rc.NodeController.Locality != nil {
		locality = rc.NodeController.Locality.Region
	}
	seEndpoint := makeRemoteEndpointForServiceEntry(endpointAddress,
		locality, finalProtocol, port)

	// if the action is deleting an endpoint from service entry, loop through the list and delete matching ones
	if event == admiral.Add || event == admiral.Update {
		tmpSe.Endpoints = append(tmpSe.Endpoints, seEndpoint)
	} else if event == admiral.Delete {
		// create a tmp endpoint list to store all the endpoints that we intend to keep
		remainEndpoints := []*networking.ServiceEntry_Endpoint{}
		// if the endpoint is not equal to the endpoint we intend to delete, append it to remainEndpoint list
		for _, existingEndpoint := range tmpSe.Endpoints {
			if !reflect.DeepEqual(existingEndpoint, seEndpoint) {
				remainEndpoints = append(remainEndpoints, existingEndpoint)
			}
		}
		// If no endpoints left for particular SE, we can delete the service entry object itself later inside function
		// AddServiceEntriesWithDr when updating SE, leave an empty shell skeleton here
		tmpSe.Endpoints = remainEndpoints
	}

	serviceEntries[globalFqdn] = tmpSe

	return tmpSe
}

func isBlueGreenStrategy(rollout *argo.Rollout) bool {
	if rollout != nil && &rollout.Spec != (&argo.RolloutSpec{}) && rollout.Spec.Strategy != (argo.RolloutStrategy{}) {
		if rollout.Spec.Strategy.BlueGreen != nil {
			return true
		}
	}
	return false
}
