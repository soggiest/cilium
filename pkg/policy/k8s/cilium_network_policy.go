// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package k8s

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/sirupsen/logrus"

	ipcacheTypes "github.com/cilium/cilium/pkg/ipcache/types"
	"github.com/cilium/cilium/pkg/k8s"
	cilium_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	cilium_v2_alpha1 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	"github.com/cilium/cilium/pkg/k8s/resource"
	slim_networking_v1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/networking/v1"
	k8sSynced "github.com/cilium/cilium/pkg/k8s/synced"
	"github.com/cilium/cilium/pkg/k8s/types"
	k8sUtils "github.com/cilium/cilium/pkg/k8s/utils"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/policy"
	"github.com/cilium/cilium/pkg/source"
	"github.com/cilium/cilium/pkg/time"
)

// ruleImportMetadataCache maps the unique identifier of a CiliumNetworkPolicy
// (namespace and name) to metadata about the importing of the rule into the
// agent's policy repository at the time said rule was imported (revision
// number, and if any error occurred while importing).
type ruleImportMetadataCache struct {
	mutex                 lock.RWMutex
	ruleImportMetadataMap map[string]policyImportMetadata
}

type policyImportMetadata struct {
	revision          uint64
	policyImportError error
}

func (r *ruleImportMetadataCache) upsert(cnp *types.SlimCNP, revision uint64, importErr error) {
	if cnp == nil {
		return
	}

	meta := policyImportMetadata{
		revision:          revision,
		policyImportError: importErr,
	}
	podNSName := k8sUtils.GetObjNamespaceName(&cnp.ObjectMeta)

	r.mutex.Lock()
	r.ruleImportMetadataMap[podNSName] = meta
	r.mutex.Unlock()
}

func (r *ruleImportMetadataCache) delete(cnp *types.SlimCNP) {
	if cnp == nil {
		return
	}
	podNSName := k8sUtils.GetObjNamespaceName(&cnp.ObjectMeta)

	r.mutex.Lock()
	delete(r.ruleImportMetadataMap, podNSName)
	r.mutex.Unlock()
}

type PolicyWatcher struct {
	k8sResourceSynced *k8sSynced.Resources
	k8sAPIGroups      *k8sSynced.APIGroups

	policyManager PolicyManager
	K8sSvcCache   *k8s.ServiceCache

	CiliumNetworkPolicies            resource.Resource[*cilium_v2.CiliumNetworkPolicy]
	CiliumClusterwideNetworkPolicies resource.Resource[*cilium_v2.CiliumClusterwideNetworkPolicy]
	CiliumCIDRGroups                 resource.Resource[*cilium_v2_alpha1.CiliumCIDRGroup]
	NetworkPolicies                  resource.Resource[*slim_networking_v1.NetworkPolicy]
}

func (p *PolicyWatcher) ciliumNetworkPoliciesInit(ctx context.Context) {
	var cnpSynced, ccnpSynced, cidrGroupSynced atomic.Bool
	go func() {
		cnpEvents := p.CiliumNetworkPolicies.Events(ctx)
		ccnpEvents := p.CiliumClusterwideNetworkPolicies.Events(ctx)

		// cnpCache contains both CNPs and CCNPs, stored using a common intermediate
		// representation (*types.SlimCNP). The cache is indexed on resource.Key,
		// that contains both the name and namespace of the resource, in order to
		// avoid key clashing between CNPs and CCNPs.
		// The cache contains CNPs and CCNPs in their "original form"
		// (i.e: pre-translation of each CIDRGroupRef to a CIDRSet).
		cnpCache := make(map[resource.Key]*types.SlimCNP)

		cidrGroupCache := make(map[string]*cilium_v2_alpha1.CiliumCIDRGroup)
		cidrGroupEvents := p.CiliumCIDRGroups.Events(ctx)

		// cidrGroupPolicies is the set of policies that are referencing CiliumCIDRGroup objects.
		cidrGroupPolicies := make(map[resource.Key]struct{})

		for {
			select {
			case event, ok := <-cnpEvents:
				if !ok {
					cnpEvents = nil
					break
				}

				if event.Kind == resource.Sync {
					cnpSynced.Store(true)
					event.Done(nil)
					continue
				}

				slimCNP := &types.SlimCNP{
					CiliumNetworkPolicy: &cilium_v2.CiliumNetworkPolicy{
						TypeMeta:   event.Object.TypeMeta,
						ObjectMeta: event.Object.ObjectMeta,
						Spec:       event.Object.Spec,
						Specs:      event.Object.Specs,
					},
				}

				resourceID := ipcacheTypes.NewResourceID(
					ipcacheTypes.ResourceKindCNP,
					slimCNP.ObjectMeta.Namespace,
					slimCNP.ObjectMeta.Name,
				)
				var err error
				switch event.Kind {
				case resource.Upsert:
					err = p.onUpsert(slimCNP, cnpCache, event.Key, cidrGroupCache, k8sAPIGroupCiliumNetworkPolicyV2, cidrGroupPolicies, resourceID)
				case resource.Delete:
					err = p.onDelete(slimCNP, cnpCache, event.Key, k8sAPIGroupCiliumNetworkPolicyV2, cidrGroupPolicies, resourceID)
				}
				reportCNPChangeMetrics(err)
				event.Done(err)
			case event, ok := <-ccnpEvents:
				if !ok {
					ccnpEvents = nil
					break
				}

				if event.Kind == resource.Sync {
					ccnpSynced.Store(true)
					event.Done(nil)
					continue
				}

				slimCNP := &types.SlimCNP{
					CiliumNetworkPolicy: &cilium_v2.CiliumNetworkPolicy{
						TypeMeta:   event.Object.TypeMeta,
						ObjectMeta: event.Object.ObjectMeta,
						Spec:       event.Object.Spec,
						Specs:      event.Object.Specs,
					},
				}

				resourceID := ipcacheTypes.NewResourceID(
					ipcacheTypes.ResourceKindCCNP,
					slimCNP.ObjectMeta.Namespace,
					slimCNP.ObjectMeta.Name,
				)
				var err error
				switch event.Kind {
				case resource.Upsert:
					err = p.onUpsert(slimCNP, cnpCache, event.Key, cidrGroupCache, k8sAPIGroupCiliumClusterwideNetworkPolicyV2, cidrGroupPolicies, resourceID)
				case resource.Delete:
					err = p.onDelete(slimCNP, cnpCache, event.Key, k8sAPIGroupCiliumClusterwideNetworkPolicyV2, cidrGroupPolicies, resourceID)
				}
				reportCNPChangeMetrics(err)
				event.Done(err)
			case event, ok := <-cidrGroupEvents:
				if !ok {
					cidrGroupEvents = nil
					break
				}

				if event.Kind == resource.Sync {
					cidrGroupSynced.Store(true)
					event.Done(nil)
					continue
				}

				var err error
				switch event.Kind {
				case resource.Upsert:
					err = p.onUpsertCIDRGroup(event.Object, cidrGroupCache, cnpCache, k8sAPIGroupCiliumCIDRGroupV2Alpha1)
				case resource.Delete:
					err = p.onDeleteCIDRGroup(event.Object.Name, cidrGroupCache, cnpCache, k8sAPIGroupCiliumCIDRGroupV2Alpha1)
				}
				event.Done(err)
			}
			if cnpEvents == nil && ccnpEvents == nil && cidrGroupEvents == nil {
				return
			}
		}
	}()

	p.registerResourceWithSyncFn(ctx, k8sAPIGroupCiliumNetworkPolicyV2, func() bool {
		return cnpSynced.Load() && cidrGroupSynced.Load()
	})
	p.registerResourceWithSyncFn(ctx, k8sAPIGroupCiliumClusterwideNetworkPolicyV2, func() bool {
		return ccnpSynced.Load() && cidrGroupSynced.Load()
	})
	p.registerResourceWithSyncFn(ctx, k8sAPIGroupCiliumCIDRGroupV2Alpha1, func() bool {
		return cidrGroupSynced.Load()
	})
}

func (p *PolicyWatcher) onUpsert(
	cnp *types.SlimCNP,
	cnpCache map[resource.Key]*types.SlimCNP,
	key resource.Key,
	cidrGroupCache map[string]*cilium_v2_alpha1.CiliumCIDRGroup,
	apiGroup string,
	cidrGroupPolicies map[resource.Key]struct{},
	resourceID ipcacheTypes.ResourceID,
) error {
	initialRecvTime := time.Now()

	defer func() {
		p.k8sResourceSynced.SetEventTimestamp(apiGroup)
	}()

	oldCNP, ok := cnpCache[key]
	if ok {
		if oldCNP.DeepEqual(cnp) {
			return nil
		}
	}

	if cnp.RequiresDerivative() {
		return nil
	}

	// check if this cnp was referencing or is now referencing at least one non-empty
	// CiliumCIDRGroup and update the relevant metric accordingly.
	cidrGroupRefs := getCIDRGroupRefs(cnp)
	cidrsSets, _ := cidrGroupRefsToCIDRsSets(cidrGroupRefs, cidrGroupCache)
	if len(cidrsSets) > 0 {
		cidrGroupPolicies[key] = struct{}{}
	} else {
		delete(cidrGroupPolicies, key)
	}
	metrics.CIDRGroupsReferenced.Set(float64(len(cidrGroupPolicies)))

	// We need to deepcopy this structure because we are writing
	// fields.
	// See https://github.com/cilium/cilium/blob/27fee207f5422c95479422162e9ea0d2f2b6c770/pkg/policy/api/ingress.go#L112-L134
	cnpCpy := cnp.DeepCopy()

	translationStart := time.Now()
	translatedCNP := resolveCIDRGroupRef(cnpCpy, cidrGroupCache)
	metrics.CIDRGroupTranslationTimeStats.Observe(time.Since(translationStart).Seconds())

	var err error
	if ok {
		err = p.updateCiliumNetworkPolicyV2(oldCNP, translatedCNP, initialRecvTime, resourceID)
	} else {
		err = p.addCiliumNetworkPolicyV2(translatedCNP, initialRecvTime, resourceID)
	}
	if err == nil {
		cnpCache[key] = cnpCpy
	}

	return err
}

func (p *PolicyWatcher) onDelete(
	cnp *types.SlimCNP,
	cache map[resource.Key]*types.SlimCNP,
	key resource.Key,
	apiGroup string,
	cidrGroupPolicies map[resource.Key]struct{},
	resourceID ipcacheTypes.ResourceID,
) error {
	err := p.deleteCiliumNetworkPolicyV2(cnp, resourceID)
	delete(cache, key)

	delete(cidrGroupPolicies, key)
	metrics.CIDRGroupsReferenced.Set(float64(len(cidrGroupPolicies)))

	p.k8sResourceSynced.SetEventTimestamp(apiGroup)

	return err
}

func (p *PolicyWatcher) addCiliumNetworkPolicyV2(cnp *types.SlimCNP, initialRecvTime time.Time, resourceID ipcacheTypes.ResourceID) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.CiliumNetworkPolicyName: cnp.ObjectMeta.Name,
		logfields.K8sAPIVersion:           cnp.TypeMeta.APIVersion,
		logfields.K8sNamespace:            cnp.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Adding CiliumNetworkPolicy")

	var rev uint64

	rules, policyImportErr := cnp.Parse()
	if policyImportErr == nil {
		policyImportErr = k8s.PreprocessRules(rules, p.K8sSvcCache)
		// Replace all rules with the same name, namespace and
		// resourceTypeCiliumNetworkPolicy
		if policyImportErr == nil {
			rev, policyImportErr = p.policyManager.PolicyAdd(rules, &policy.AddOptions{
				ReplaceWithLabels:   cnp.GetIdentityLabels(),
				Source:              source.CustomResource,
				ProcessingStartTime: initialRecvTime,
				Resource:            resourceID,
			})
		}
	}

	if policyImportErr != nil {
		scopedLog.WithError(policyImportErr).Warn("Unable to add CiliumNetworkPolicy")
	} else {
		scopedLog.Info("Imported CiliumNetworkPolicy")
	}

	// Upsert to rule revision cache outside of controller, because upsertion
	// *must* be synchronous so that if we get an update for the CNP, the cache
	// is populated by the time updateCiliumNetworkPolicyV2 is invoked.
	importMetadataCache.upsert(cnp, rev, policyImportErr)

	return policyImportErr
}

func (p *PolicyWatcher) deleteCiliumNetworkPolicyV2(cnp *types.SlimCNP, resourceID ipcacheTypes.ResourceID) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.CiliumNetworkPolicyName: cnp.ObjectMeta.Name,
		logfields.K8sAPIVersion:           cnp.TypeMeta.APIVersion,
		logfields.K8sNamespace:            cnp.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Deleting CiliumNetworkPolicy")

	importMetadataCache.delete(cnp)
	ctrlName := cnp.GetControllerName()
	err := k8sCM.RemoveControllerAndWait(ctrlName)
	if err != nil {
		log.WithError(err).Debugf("Unable to remove controller %s", ctrlName)
	}

	_, err = p.policyManager.PolicyDelete(cnp.GetIdentityLabels(), &policy.DeleteOptions{
		Source:   source.CustomResource,
		Resource: resourceID,
	})
	if err == nil {
		scopedLog.Info("Deleted CiliumNetworkPolicy")
	} else {
		scopedLog.WithError(err).Warn("Unable to delete CiliumNetworkPolicy")
	}
	return err
}

func (p *PolicyWatcher) updateCiliumNetworkPolicyV2(
	oldRuleCpy, newRuleCpy *types.SlimCNP, initialRecvTime time.Time, resourceID ipcacheTypes.ResourceID) error {

	_, err := oldRuleCpy.Parse()
	if err != nil {
		ns := oldRuleCpy.GetNamespace() // Disambiguates CNP & CCNP

		// We want to ignore parsing errors for empty policies, otherwise the
		// update to the new policy will be skipped.
		switch {
		case ns != "" && !errors.Is(err, cilium_v2.ErrEmptyCNP):
			log.WithError(err).WithField(logfields.Object, logfields.Repr(oldRuleCpy)).
				Warn("Error parsing old CiliumNetworkPolicy rule")
			return err
		case ns == "" && !errors.Is(err, cilium_v2.ErrEmptyCCNP):
			log.WithError(err).WithField(logfields.Object, logfields.Repr(oldRuleCpy)).
				Warn("Error parsing old CiliumClusterwideNetworkPolicy rule")
			return err
		}
	}

	_, err = newRuleCpy.Parse()
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(newRuleCpy)).
			Warn("Error parsing new CiliumNetworkPolicy rule")
		return err
	}

	log.WithFields(logrus.Fields{
		logfields.K8sAPIVersion:                    oldRuleCpy.TypeMeta.APIVersion,
		logfields.CiliumNetworkPolicyName + ".old": oldRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace + ".old":            oldRuleCpy.ObjectMeta.Namespace,
		logfields.CiliumNetworkPolicyName:          newRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace:                     newRuleCpy.ObjectMeta.Namespace,
		"annotations.old":                          oldRuleCpy.ObjectMeta.Annotations,
		"annotations":                              newRuleCpy.ObjectMeta.Annotations,
	}).Debug("Modified CiliumNetworkPolicy")

	return p.addCiliumNetworkPolicyV2(newRuleCpy, initialRecvTime, resourceID)
}

func (p *PolicyWatcher) registerResourceWithSyncFn(ctx context.Context, resource string, syncFn func() bool) {
	p.k8sResourceSynced.BlockWaitGroupToSyncResources(ctx.Done(), nil, syncFn, resource)
	p.k8sAPIGroups.AddAPI(resource)
}

// reportCNPChangeMetrics generates metrics for changes (Add, Update, Delete) to
// Cilium Network Policies depending on the operation's success.
func reportCNPChangeMetrics(err error) {
	if err != nil {
		metrics.PolicyChangeTotal.WithLabelValues(metrics.LabelValueOutcomeFail).Inc()
	} else {
		metrics.PolicyChangeTotal.WithLabelValues(metrics.LabelValueOutcomeSuccess).Inc()
	}
}
