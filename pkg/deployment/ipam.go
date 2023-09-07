/*
Copyright 2023.

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

package deployment

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dataplanev1 "github.com/openstack-k8s-operators/dataplane-operator/api/v1beta1"
	infranetworkv1 "github.com/openstack-k8s-operators/infra-operator/apis/network/v1beta1"
	condition "github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"
)

// EnsureIPSets Creates the IPSets
func EnsureIPSets(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet) (map[string]infranetworkv1.IPSet, bool, error) {
	allIPSets, err := reserveIPs(ctx, helper, instance)
	if err != nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetIPReservationReadyCondition,
			condition.ErrorReason, condition.SeverityError,
			dataplanev1.NodeSetIPReservationReadyErrorMessage)
		return nil, false, err
	}

	if len(allIPSets) == 0 {
		return nil, true, nil
	}

	for _, s := range allIPSets {
		if s.Status.Conditions.IsFalse(condition.ReadyCondition) {
			instance.Status.Conditions.MarkFalse(
				dataplanev1.NodeSetIPReservationReadyCondition,
				condition.RequestedReason, condition.SeverityInfo,
				dataplanev1.NodeSetIPReservationReadyWaitingMessage)
			return nil, false, nil
		}
	}
	instance.Status.Conditions.MarkTrue(
		dataplanev1.NodeSetIPReservationReadyCondition,
		dataplanev1.NodeSetIPReservationReadyMessage)
	return allIPSets, true, nil

}

// createOrPatchDNSData builds the DNSData
func createOrPatchDNSData(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet,
	allIPSets map[string]infranetworkv1.IPSet) (string, error) {

	var allDNSRecords []infranetworkv1.DNSHost
	var ctlplaneSearchDomain string
	// Build DNSData CR
	for nodeName, node := range instance.Spec.NodeTemplate.Nodes {
		nets := node.Networks
		if len(nets) == 0 {
			nets = instance.Spec.NodeTemplate.Networks
		}
		if len(nets) > 0 {
			// Get IPSet
			ipSet, ok := allIPSets[nodeName]
			if ok {
				for _, res := range ipSet.Status.Reservation {
					dnsRecord := infranetworkv1.DNSHost{}
					dnsRecord.IP = res.Address
					var fqdnNames []string
					fqdnName := strings.Join([]string{nodeName, res.DNSDomain}, ".")
					fqdnNames = append(fqdnNames, fqdnName)
					dnsRecord.Hostnames = fqdnNames
					allDNSRecords = append(allDNSRecords, dnsRecord)
					// Adding only ctlplane domain for ansibleee.
					// TODO (rabi) This is not very efficient.
					if res.Network == CtlPlaneNetwork && ctlplaneSearchDomain == "" {
						ctlplaneSearchDomain = res.DNSDomain
					}
				}
			}
		}
	}
	util.LogForObject(helper, "Reconciling DNSData", instance)
	dnsData := &infranetworkv1.DNSData{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: instance.Namespace,
			Name:      instance.Name,
		},
	}
	_, err := controllerutil.CreateOrPatch(ctx, helper.GetClient(), dnsData, func() error {
		dnsData.Spec.Hosts = allDNSRecords
		// TODO (rabi) DNSDataLabelSelectorValue can probably be
		// used from dnsmasq(?)
		dnsData.Spec.DNSDataLabelSelectorValue = "dnsdata"
		// Set controller reference to the DataPlaneNode object
		err := controllerutil.SetControllerReference(
			helper.GetBeforeObject(), dnsData, helper.GetScheme())
		return err
	})
	if err != nil {
		return "", err
	}
	return ctlplaneSearchDomain, nil

}

// EnsureDNSData Ensures DNSData is created
func EnsureDNSData(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet,
	allIPSets map[string]infranetworkv1.IPSet) ([]string, []string, string, bool, error) {

	// Verify dnsmasq CR exists
	dnsAddresses, dnsClusterAddresses, isReady, err := checkDNSService(
		ctx, helper, instance)

	if err != nil || !isReady || dnsAddresses == nil {
		if err != nil {
			instance.Status.Conditions.MarkFalse(
				dataplanev1.NodeSetDNSDataReadyCondition,
				condition.ErrorReason, condition.SeverityError,
				dataplanev1.NodeSetDNSDataReadyErrorMessage)
		}
		if !isReady {
			instance.Status.Conditions.MarkFalse(
				dataplanev1.NodeSetDNSDataReadyCondition,
				condition.RequestedReason, condition.SeverityInfo,
				dataplanev1.NodeSetDNSDataReadyWaitingMessage)
		}
		if dnsAddresses == nil {
			instance.Status.Conditions.Remove(dataplanev1.NodeSetDNSDataReadyCondition)
		}
		return nil, nil, "", isReady, err
	}
	// Create or Patch DNSData
	ctlplaneSearchDomain, err := createOrPatchDNSData(
		ctx, helper, instance, allIPSets)
	if err != nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.ErrorReason, condition.SeverityError,
			dataplanev1.NodeSetDNSDataReadyErrorMessage)
		return nil, nil, "", false, err
	}

	dnsData := &infranetworkv1.DNSData{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}
	key := client.ObjectKeyFromObject(dnsData)
	err = helper.GetClient().Get(ctx, key, dnsData)
	if err != nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.ErrorReason, condition.SeverityError,
			dataplanev1.NodeSetDNSDataReadyErrorMessage)
		return nil, nil, "", false, err
	}

	if !dnsData.IsReady() {
		util.LogForObject(helper, "DNSData not ready yet waiting", instance)
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.RequestedReason, condition.SeverityInfo,
			dataplanev1.NodeSetDNSDataReadyWaitingMessage)
		return nil, nil, "", false, nil
	}
	instance.Status.Conditions.MarkTrue(
		dataplanev1.NodeSetDNSDataReadyCondition,
		dataplanev1.NodeSetDNSDataReadyMessage)
	return dnsAddresses, dnsClusterAddresses, ctlplaneSearchDomain, true, nil
}

// reserveIPs Reserves IPs by creating IPSets
func reserveIPs(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet) (map[string]infranetworkv1.IPSet, error) {

	// Verify NetConfig CRs exist
	netConfigList := &infranetworkv1.NetConfigList{}
	listOpts := []client.ListOption{
		client.InNamespace(instance.GetNamespace()),
	}
	err := helper.GetClient().List(ctx, netConfigList, listOpts...)
	if err != nil {
		return nil, err
	}
	if len(netConfigList.Items) == 0 {
		util.LogForObject(helper, "No NetConfig CR exists yet, IPAM won't be used", instance)
		instance.Status.Conditions.Remove(dataplanev1.NodeSetIPReservationReadyCondition)
		return nil, nil
	}

	allIPSets := make(map[string]infranetworkv1.IPSet)
	// CreateOrPatch IPSets
	for nodeName, node := range instance.Spec.NodeTemplate.Nodes {
		nets := node.Networks

		if len(nets) == 0 {
			nets = instance.Spec.NodeTemplate.Networks
		}

		if len(nets) > 0 {
			util.LogForObject(helper, "Reconciling IPSet", instance)
			ipSet := &infranetworkv1.IPSet{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: instance.Namespace,
					Name:      nodeName,
				},
			}
			_, err := controllerutil.CreateOrPatch(ctx, helper.GetClient(), ipSet, func() error {
				ipSet.Spec.Networks = nets
				// Set controller reference to the DataPlaneNode object
				err := controllerutil.SetControllerReference(
					helper.GetBeforeObject(), ipSet, helper.GetScheme())
				return err
			})
			if err != nil {
				return nil, err
			}
			allIPSets[nodeName] = *ipSet
		}
	}

	return allIPSets, nil
}

// checkDNSService checks if DNS is configured and ready
func checkDNSService(ctx context.Context, helper *helper.Helper,
	instance client.Object) ([]string, []string, bool, error) {
	dnsmasqList := &infranetworkv1.DNSMasqList{}
	listOpts := []client.ListOption{
		client.InNamespace(instance.GetNamespace()),
	}
	err := helper.GetClient().List(ctx, dnsmasqList, listOpts...)
	if err != nil {
		return nil, nil, false, err
	}
	if len(dnsmasqList.Items) == 0 {
		util.LogForObject(helper, "No DNSMasq CR exists yet, DNS Service won't be used", instance)
		return nil, nil, true, nil
	} else if !dnsmasqList.Items[0].IsReady() {
		util.LogForObject(helper, "DNSMasq service exists, but not ready yet ", instance)
		return nil, nil, false, nil
	}
	dnsClusterAddresses := dnsmasqList.Items[0].Status.DNSClusterAddresses
	dnsAddresses := dnsmasqList.Items[0].Status.DNSAddresses
	return dnsAddresses, dnsClusterAddresses, true, nil
}
