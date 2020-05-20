/*
Copyright 2020 The OpenEBS Authors

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

package migrate

import (
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"

	cstor "github.com/openebs/api/pkg/apis/cstor/v1"
	"github.com/openebs/api/pkg/apis/types"
	openebsclientset "github.com/openebs/api/pkg/client/clientset/versioned"
	apis "github.com/openebs/maya/pkg/apis/openebs.io/v1alpha1"
	csp "github.com/openebs/maya/pkg/cstor/pool/v1alpha3"
	cvr "github.com/openebs/maya/pkg/cstor/volumereplica/v1alpha1"
	spc "github.com/openebs/maya/pkg/storagepoolclaim/v1alpha1"
	"github.com/openebs/maya/pkg/util/retry"
	"github.com/pkg/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

const (
	replicaPatch = `{
	"spec": {
		"replicas": 0
	}
}`
	cspNameLabel           = "cstorpool.openebs.io/name"
	cspUIDLabel            = "cstorpool.openebs.io/uid"
	cspHostnameAnnotation  = "cstorpool.openebs.io/hostname"
	cspiNameLabel          = "cstorpoolinstance.openebs.io/name"
	cspiUIDLabel           = "cstorpoolinstance.openebs.io/uid"
	cspiHostnameAnnotation = "cstorpoolinstance.openebs.io/hostname"
	spcFinalizer           = "storagepoolclaim.openebs.io/finalizer"
	cspcFinalizer          = "cstorpoolcluster.openebs.io/finalizer"
)

// CSPCMigrator ...
type CSPCMigrator struct {
	// kubeclientset is a standard kubernetes clientset
	KubeClientset kubernetes.Interface
	// openebsclientset is a openebs custom resource package generated for custom API group.
	OpenebsClientset openebsclientset.Interface
	CSPCObj          *cstor.CStorPoolCluster
}

// Migrate ...
func (c *CSPCMigrator) Migrate(name, namespace string) error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return errors.Wrap(err, "error building kubeconfig")
	}
	c.KubeClientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "error building kubernetes clientset")
	}
	c.OpenebsClientset, err = openebsclientset.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "error building openebs clientset")
	}
	err = c.migrate(name, namespace)
	return err
}

// Pool migrates the pool from SPC schema to CSPC schema
func (c *CSPCMigrator) migrate(spcName, openebsNamespace string) error {
	spcObj, migrated, err := c.getSPCWithMigrationStatus(spcName, openebsNamespace)
	if migrated {
		klog.Infof("spc %s is already migrated to cspc", spcName)
		return nil
	}
	if err != nil {
		return err
	}
	err = validateSPC(spcObj)
	if err != nil {
		return err
	}
	err = c.updateBDCLabels(spcName, openebsNamespace)
	if err != nil {
		return err
	}
	klog.Infof("Creating equivalent cspc for spc %s", spcName)
	cspcObj, err := c.generateCSPC(spcObj, openebsNamespace)
	if err != nil {
		return err
	}
	err = c.updateBDCOwnerRef(cspcObj, openebsNamespace)
	if err != nil {
		return err
	}
	// List all cspi created with reconcile off
	cspiList, err := c.OpenebsClientset.CstorV1().
		CStorPoolInstances(openebsNamespace).
		List(metav1.ListOptions{
			LabelSelector: string(apis.CStorPoolClusterCPK) + "=" + cspcObj.Name,
		})
	if err != nil {
		return err
	}

	// For each cspi perform the migration from csp that present was on
	// node on which cspi came up.
	for _, cspiItem := range cspiList.Items {
		// Skip the migration if cspi is already in ONLINE state,
		// which implies the migration is done and makes it idempotent
		cspiItem := cspiItem // pin it
		cspiObj := &cspiItem
		if cspiObj.Status.Phase != "ONLINE" {
			err = c.csptocspi(cspiObj, cspcObj, openebsNamespace)
			if err != nil {
				return err
			}
		}
	}
	// Clean up old SPC resources after the migration is complete
	err = spc.NewKubeClient().
		Delete(spcName, &metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	return nil
}

// validateSPC determines that if the spc is allowed to migrate or not.
// If the max pool count does not match the number of csp for auto spc, or
// the bd list in spc does not match bds from the csp migration should not be done.
func validateSPC(spcObj *apis.StoragePoolClaim) error {
	cspClient := csp.KubeClient()
	cspList, err := cspClient.List(metav1.ListOptions{
		LabelSelector: string(apis.StoragePoolClaimCPK) + "=" + spcObj.Name,
	})
	if err != nil {
		return err
	}
	if spcObj.Spec.BlockDevices.BlockDeviceList == nil {
		if spcObj.Spec.MaxPools == nil {
			return errors.Errorf("invalid spc %s neither has bdc list nor maxpools", spcObj.Name)
		}
		if *spcObj.Spec.MaxPools != len(cspList.Items) {
			return errors.Errorf("maxpool count does not match csp count expected: %d got: %d",
				*spcObj.Spec.MaxPools, len(cspList.Items))
		}
		return nil
	}
	bdMap := map[string]int{}
	for _, bdName := range spcObj.Spec.BlockDevices.BlockDeviceList {
		bdMap[bdName]++
	}
	for _, cspObj := range cspList.Items {
		for _, bdObj := range cspObj.Spec.Group[0].Item {
			bdMap[bdObj.Name]++
		}
	}
	for bdName, count := range bdMap {
		// if bd is configured properly it should occur exactly twice
		// one in spc spec and one in csp spec
		if count != 2 {
			return errors.Errorf("bd %s is not configured properly", bdName)
		}
	}
	return nil
}

// getSPCWithMigrationStatus gets the spc by name and verifies if the spc is already
// migrated or not. The spc will not be present in the cluster as the last step
// of migration deletes the spc.
func (c *CSPCMigrator) getSPCWithMigrationStatus(spcName, openebsNamespace string) (*apis.StoragePoolClaim, bool, error) {
	spcObj, err := spc.NewKubeClient().
		Get(spcName, metav1.GetOptions{})
	// verify if the spc is already migrated. If an equivalent cspc exists then
	// spc is already migrated as spc is only deleted as last step.
	if k8serrors.IsNotFound(err) {
		klog.Infof("spc %s not found.", spcName)
		_, err = c.OpenebsClientset.CstorV1().
			CStorPoolClusters(openebsNamespace).Get(spcName, metav1.GetOptions{})
		if err != nil {
			return nil, false, errors.Wrapf(err, "failed to get equivalent cspc for spc %s", spcName)
		}
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return spcObj, false, nil
}

// csptocspi migrates a CSP to CSPI based on hostname
func (c *CSPCMigrator) csptocspi(
	cspiObj *cstor.CStorPoolInstance,
	cspcObj *cstor.CStorPoolCluster,
	openebsNamespace string,
) error {
	hostnameLabel := string(apis.HostNameCPK) + "=" + cspiObj.Labels[string(apis.HostNameCPK)]
	spcLabel := string(apis.StoragePoolClaimCPK) + "=" + cspcObj.Name
	cspLabel := hostnameLabel + "," + spcLabel
	var err1 error
	cspObj, err := getCSP(cspLabel)
	if err != nil {
		return err
	}
	klog.Infof("Migrating csp %s to cspi %s", cspiObj.Name, cspObj.Name)
	err = c.scaleDownDeployment(cspObj, openebsNamespace)
	if err != nil {
		return err
	}
	// once the old pool pod is scaled down and bdcs are patched
	// bring up the cspi pod so that the old pool can be renamed and imported.
	cspiObj.Annotations[types.OldPoolName] = "cstor-" + string(cspObj.UID)
	delete(cspiObj.Annotations, types.OpenEBSDisableReconcileLabelKey)
	cspiObj, err = c.OpenebsClientset.CstorV1().
		CStorPoolInstances(openebsNamespace).
		Update(cspiObj)
	if err != nil {
		return err
	}
	err = retry.
		Times(60).
		Wait(5 * time.Second).
		Try(func(attempt uint) error {
			cspiObj, err1 = c.OpenebsClientset.CstorV1().
				CStorPoolInstances(openebsNamespace).
				Get(cspiObj.Name, metav1.GetOptions{})
			if err1 != nil {
				return err1
			}
			if cspiObj.Status.Phase != "ONLINE" {
				return errors.Errorf("failed to verify cspi %s phase expected: Healthy got: %s",
					cspiObj.Name, cspiObj.Status.Phase)
			}
			return nil
		})
	if err != nil {
		return err
	}
	err = c.updateCVRsLabels(cspObj.Name, openebsNamespace, cspiObj)
	if err != nil {
		return err
	}
	return nil
}

// get csp for cspi on the basis of cspLabel, which is the combination of
// hostname label on which cspi came up and the spc label.
func getCSP(cspLabel string) (*apis.CStorPool, error) {
	cspClient := csp.KubeClient()
	cspList, err := cspClient.List(metav1.ListOptions{
		LabelSelector: cspLabel,
	})
	if err != nil {
		return nil, err
	}
	if len(cspList.Items) != 1 {
		return nil, fmt.Errorf("Invalid number of pools on one node: %v", cspList.Items)
	}
	cspObj := cspList.Items[0]
	return &cspObj, nil
}

// The old pool pod should be scaled down before the new cspi pod reconcile is
// enabled to avoid importing the pool at two places at the same time.
func (c *CSPCMigrator) scaleDownDeployment(cspObj *apis.CStorPool, openebsNamespace string) error {
	klog.Infof("Scaling down deployemnt %s", cspObj.Name)
	cspDeployList, err := c.KubeClientset.AppsV1().
		Deployments(openebsNamespace).List(
		metav1.ListOptions{
			LabelSelector: "openebs.io/cstor-pool=" + cspObj.Name,
		})
	if err != nil {
		return err
	}
	if len(cspDeployList.Items) != 1 {
		return errors.Errorf("invalid number of csp deployment found for %s: %d", cspObj.Name, len(cspDeployList.Items))
	}
	_, err = c.KubeClientset.AppsV1().Deployments(openebsNamespace).
		Patch(
			cspDeployList.Items[0].Name,
			k8stypes.StrategicMergePatchType,
			[]byte(replicaPatch),
		)
	if err != nil {
		return err
	}
	err = retry.
		Times(60).
		Wait(5 * time.Second).
		Try(func(attempt uint) error {
			cspDeploy, err1 := c.KubeClientset.AppsV1().
				Deployments(openebsNamespace).
				Get(cspDeployList.Items[0].Name, metav1.GetOptions{})
			if err1 != nil {
				return errors.Wrapf(err1, "failed to get csp deploy")
			}
			if cspDeploy.Status.ReadyReplicas != 0 {
				return errors.Errorf("failed to scale down csp deployment")
			}
			return nil
		})
	return err
}

// Update the bdc with the cspc labels instead of spc labels to allow
// filtering of bds claimed by the migrated cspc.
func (c *CSPCMigrator) updateBDCLabels(cspcName, openebsNamespace string) error {
	bdcList, err := c.OpenebsClientset.OpenebsV1alpha1().BlockDeviceClaims(openebsNamespace).List(metav1.ListOptions{
		LabelSelector: string(apis.StoragePoolClaimCPK) + "=" + cspcName,
	})
	if err != nil {
		return err
	}
	for _, bdcItem := range bdcList.Items {
		if bdcItem.Labels[string(apis.StoragePoolClaimCPK)] != "" {
			bdcItem := bdcItem // pin it
			bdcObj := &bdcItem
			klog.Infof("Updating bdc %s with cspc labels & finalizer.", bdcObj.Name)
			delete(bdcObj.Labels, string(apis.StoragePoolClaimCPK))
			bdcObj.Labels[types.CStorPoolClusterLabelKey] = cspcName
			for i, finalizer := range bdcObj.Finalizers {
				if finalizer == spcFinalizer {
					bdcObj.Finalizers[i] = cspcFinalizer
				}
			}
			_, err := c.OpenebsClientset.OpenebsV1alpha1().BlockDeviceClaims(openebsNamespace).
				Update(bdcObj)
			if err != nil {
				return errors.Wrapf(err, "failed to update bdc %s with cspc label & finalizer", bdcObj.Name)
			}
		}
	}
	return nil
}

// Update the bdc with the cspc OwnerReferences instead of spc OwnerReferences
// to allow clean up of bdcs on deletion of cspc.
func (c *CSPCMigrator) updateBDCOwnerRef(cspcObj *cstor.CStorPoolCluster, openebsNamespace string) error {
	bdcList, err := c.OpenebsClientset.OpenebsV1alpha1().
		BlockDeviceClaims(openebsNamespace).
		List(metav1.ListOptions{
			LabelSelector: types.CStorPoolClusterLabelKey + "=" + cspcObj.Name,
		})
	if err != nil {
		return err
	}
	for _, bdcItem := range bdcList.Items {
		if bdcItem.OwnerReferences[0].Kind != "CStorPoolCluster" {
			bdcItem := bdcItem // pin it
			bdcObj := &bdcItem
			klog.Infof("Updating bdc %s with cspc ownerRef.", bdcObj.Name)
			bdcObj.OwnerReferences[0].Kind = "CStorPoolCluster"
			bdcObj.OwnerReferences[0].UID = cspcObj.UID
			bdcObj.OwnerReferences[0].APIVersion = "cstor.openebs.io/v1"
			_, err := c.OpenebsClientset.OpenebsV1alpha1().BlockDeviceClaims(openebsNamespace).
				Update(bdcObj)
			if err != nil {
				return errors.Wrapf(err, "failed to update bdc %s with cspc onwerRef", bdcObj.Name)
			}
		}
	}
	return nil
}

// Update the cvrs on the old csp with the migrated cspi labels and annotations
// to allow backward compatibility with old external provisioned volumes.
func (c *CSPCMigrator) updateCVRsLabels(cspName, openebsNamespace string, cspiObj *cstor.CStorPoolInstance) error {
	cvrList, err := cvr.NewKubeclient().
		WithNamespace(openebsNamespace).List(metav1.ListOptions{
		LabelSelector: cspNameLabel + "=" + cspName,
	})
	if err != nil {
		return err
	}
	for _, cvrItem := range cvrList.Items {
		if cvrItem.Labels[cspiNameLabel] == "" {
			cvrItem := cvrItem // pin it
			cvrObj := &cvrItem
			klog.Infof("Updating cvr %s with cspi %s info.", cvrObj.Name, cspiObj.Name)
			delete(cvrObj.Labels, cspNameLabel)
			delete(cvrObj.Labels, cspUIDLabel)
			cvrObj.Labels[cspiNameLabel] = cspiObj.Name
			cvrObj.Labels[cspiUIDLabel] = string(cspiObj.UID)
			delete(cvrObj.Annotations, cspHostnameAnnotation)
			cvrObj.Annotations[cspiHostnameAnnotation] = cspiObj.Spec.HostName
			_, err = cvr.NewKubeclient().WithNamespace(openebsNamespace).
				Update(cvrObj)
			if err != nil {
				return errors.Wrapf(err, "failed to update cvr %s with cspc info", cvrObj.Name)
			}
		}
	}
	return nil
}
