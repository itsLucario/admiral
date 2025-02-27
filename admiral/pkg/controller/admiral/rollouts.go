package admiral

import (
	"fmt"
	"sync"
	"time"

	argo "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	argoclientset "github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned"
	argoprojv1alpha1 "github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned/typed/rollouts/v1alpha1"
	argoinformers "github.com/argoproj/argo-rollouts/pkg/client/informers/externalversions"
	"github.com/istio-ecosystem/admiral/admiral/pkg/controller/common"
	"github.com/prometheus/common/log"
	"github.com/sirupsen/logrus"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Handler interface contains the methods that are required
type RolloutHandler interface {
	Added(obj *argo.Rollout)
	Updated(obj *argo.Rollout)
	Deleted(obj *argo.Rollout)
}

type RolloutsEntry struct {
	Identity string
	Rollout  *argo.Rollout
}

type RolloutClusterEntry struct {
	Identity string
	Rollouts map[string]*argo.Rollout
}

type RolloutController struct {
	K8sClient      kubernetes.Interface
	RolloutClient  argoprojv1alpha1.ArgoprojV1alpha1Interface
	RolloutHandler RolloutHandler
	informer       cache.SharedIndexInformer
	Cache          *rolloutCache
	labelSet       *common.LabelSet
}

type rolloutCache struct {
	//map of dependencies key=identity value array of onboarded identities
	cache map[string]*RolloutClusterEntry
	mutex *sync.Mutex
}

func (p *rolloutCache) Put(rolloutEntry *RolloutClusterEntry) {
	defer p.mutex.Unlock()
	p.mutex.Lock()

	p.cache[rolloutEntry.Identity] = rolloutEntry
}

func (p *rolloutCache) getKey(rollout *argo.Rollout) string {
	return common.GetRolloutGlobalIdentifier(rollout)
}

func (p *rolloutCache) Get(key string, env string) *argo.Rollout {
	defer p.mutex.Unlock()
	p.mutex.Lock()
	rce := p.cache[key]
	if rce == nil {
		return nil
	} else {
		return rce.Rollouts[env]
	}
}

func (p *rolloutCache) Delete(pod *RolloutClusterEntry) {
	defer p.mutex.Unlock()
	p.mutex.Lock()
	delete(p.cache, pod.Identity)
}

func (p *rolloutCache) UpdateRolloutToClusterCache(key string, rollout *argo.Rollout) {
	defer p.mutex.Unlock()
	p.mutex.Lock()

	env := common.GetEnvForRollout(rollout)

	rce := p.cache[key]

	if rce == nil {
		rce = &RolloutClusterEntry{
			Identity: key,
			Rollouts: make(map[string]*argo.Rollout),
		}
	}
	rce.Rollouts[env] = rollout
	p.cache[rce.Identity] = rce
}

func (p *rolloutCache) DeleteFromRolloutToClusterCache(key string, rollout *argo.Rollout) {
	defer p.mutex.Unlock()
	p.mutex.Lock()

	env := common.GetEnvForRollout(rollout)

	rce := p.cache[key]

	if rce != nil {
		delete(rce.Rollouts, env)
	}
}

func (d *RolloutController) shouldIgnoreBasedOnLabelsForRollout(rollout *argo.Rollout) bool {
	if rollout.Spec.Template.Labels[d.labelSet.AdmiralIgnoreLabel] == "true" { //if we should ignore, do that and who cares what else is there
		return true
	}

	if rollout.Spec.Template.Annotations[d.labelSet.DeploymentAnnotation] != "true" { //Not sidecar injected, we don't want to inject
		return true
	}

	if rollout.Annotations[common.AdmiralIgnoreAnnotation] == "true" {
		return true
	}

	ns, err := d.K8sClient.CoreV1().Namespaces().Get(rollout.Namespace, meta_v1.GetOptions{})
	if err != nil {
		log.Warnf("Failed to get namespace object for rollout with namespace %v, err: %v", rollout.Namespace, err)
		return false
	}

	if ns.Annotations[common.AdmiralIgnoreAnnotation] == "true" {
		return true
	}
	return false //labels are fine, we should not ignore
}

func NewRolloutsController(clusterID string, stopCh <-chan struct{}, handler RolloutHandler, config *rest.Config, resyncPeriod time.Duration) (*RolloutController, error) {

	roController := RolloutController{}
	roController.RolloutHandler = handler
	roController.labelSet = common.GetLabelSet()

	rolloutCache := rolloutCache{}
	rolloutCache.cache = make(map[string]*RolloutClusterEntry)
	rolloutCache.mutex = &sync.Mutex{}

	roController.Cache = &rolloutCache

	var err error
	rolloutClient, err := argoclientset.NewForConfig(config)

	if err != nil {
		return nil, fmt.Errorf("failed to create rollouts controller argo client: %v", err)
	}

	roController.K8sClient, err = K8sClientFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create rollouts controller k8s client: %v", err)
	}

	roController.RolloutClient = rolloutClient.ArgoprojV1alpha1()

	argoRolloutsInformerFactory := argoinformers.NewSharedInformerFactoryWithOptions(
		rolloutClient,
		resyncPeriod,
		argoinformers.WithNamespace(meta_v1.NamespaceAll))
	//Initialize informer
	roController.informer = argoRolloutsInformerFactory.Argoproj().V1alpha1().Rollouts().Informer()

	mcd := NewMonitoredDelegator(&roController, clusterID, "rollout")
	NewController("rollouts-ctrl-"+clusterID, stopCh, mcd, roController.informer)
	return &roController, nil
}

func (roc *RolloutController) Added(ojb interface{}) {
	HandleAddUpdateRollout(ojb, roc)
}

func (roc *RolloutController) Updated(ojb interface{}, oldObj interface{}) {
	HandleAddUpdateRollout(ojb, roc)
}

func HandleAddUpdateRollout(ojb interface{}, roc *RolloutController) {
	rollout := ojb.(*argo.Rollout)
	key := roc.Cache.getKey(rollout)
	if len(key) > 0 {
		if !roc.shouldIgnoreBasedOnLabelsForRollout(rollout) {
			roc.Cache.UpdateRolloutToClusterCache(key, rollout)
			roc.RolloutHandler.Added(rollout)
		} else {
			roc.Cache.DeleteFromRolloutToClusterCache(key, rollout)
			log.Debugf("ignoring rollout %v based on labels", rollout.Name)
		}
	}
}

func (roc *RolloutController) Deleted(ojb interface{}) {
	rollout := ojb.(*argo.Rollout)
	key := roc.Cache.getKey(rollout)
	if len(key) > 0 {
		roc.Cache.DeleteFromRolloutToClusterCache(key, rollout)
	}
	roc.RolloutHandler.Deleted(rollout)
}

func (d *RolloutController) GetRolloutBySelectorInNamespace(serviceSelector map[string]string, namespace string) []argo.Rollout {

	matchedRollouts, err := d.RolloutClient.Rollouts(namespace).List(meta_v1.ListOptions{})

	if err != nil {
		logrus.Errorf("Failed to list rollouts in cluster, error: %v", err)
		return nil
	}

	if matchedRollouts.Items == nil {
		return make([]argo.Rollout, 0)
	}

	filteredRollouts := make([]argo.Rollout, 0)

	for _, rollout := range matchedRollouts.Items {
		if common.IsServiceMatch(serviceSelector, rollout.Spec.Selector) {
			filteredRollouts = append(filteredRollouts, rollout)
		}
	}

	return filteredRollouts
}
